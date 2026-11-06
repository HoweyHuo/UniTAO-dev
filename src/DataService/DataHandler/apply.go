package DataHandler

import (
	"fmt"
	"net/http"

	"DataService/Common"

	"github.com/salesforce/UniTAO/lib/Schema"
	"github.com/salesforce/UniTAO/lib/Schema/JsonKey"
	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util"
	"github.com/salesforce/UniTAO/lib/Util/Http"
)

// 以下方法供 mirrored vDS 应用 Channel 变更使用：绕过只读限制，不做引用校验，不写 Journal。
// 来源已经在写入时完成了校验，并且通道内容经过签名，镜像侧只保证记录格式与 schema 有效。

// IsSyncedRecord 判断一条记录是否属于需要经 Channel 同步的数据：
// 内部类型（journal、outbox、cmtIdx 等）不同步；schema 记录中内部类型自身的 schema 不同步。
func IsSyncedRecord(dataType string, dataId string) bool {
	if dataType == JsonKey.Schema {
		schemaName, _ := Util.ParseCustomPath(dataId, JsonKey.ArchivedSchemaIdDiv)
		_, internal := Common.InternalTypes[schemaName]
		return !internal
	}
	_, internal := Common.InternalTypes[dataType]
	return !internal
}

// ApplyUpsert 写入（新增或覆盖）一条来自 Channel 的记录。
func (h *Handler) ApplyUpsert(data map[string]interface{}) *Http.HttpError {
	record, ex := Record.LoadMap(data)
	if ex != nil {
		return Http.WrapError(ex, "invalid record from channel", http.StatusBadRequest)
	}
	if ex := record.ValidateEnvelope(); ex != nil {
		return Http.WrapError(ex, "invalid record from channel", http.StatusBadRequest)
	}
	if !IsSyncedRecord(record.Type, record.Id) {
		return Http.NewHttpError(fmt.Sprintf("record [%s/%s] is internal and cannot be applied from channel", record.Type, record.Id), http.StatusBadRequest)
	}
	isSchema := record.Type == JsonKey.Schema
	if isSchema {
		if _, ex := Schema.LoadSchemaOpsRecord(record); ex != nil {
			return Http.WrapError(ex, fmt.Sprintf("invalid schema [%s] from channel", record.Id), http.StatusBadRequest)
		}
	} else {
		schema, err := h.LocalSchema(record.Type, record.Version)
		if err != nil {
			return err
		}
		if ex := schema.ValidateRecord(record); ex != nil {
			return Http.WrapError(ex, fmt.Sprintf("record [%s/%s] from channel failed schema validation", record.Type, record.Id), http.StatusBadRequest)
		}
	}
	idKey := fmt.Sprintf("%s/%s", record.Type, record.Id)
	h.Lock.Aquire(idKey, "HandlerApply")
	defer h.Lock.Release(idKey, "HandlerApply")
	existing, err := h.QueryDb(record.Type, record.Id)
	if err != nil {
		return err
	}
	e := h.DB.Replace(h.Config.DataTable.Data, map[string]interface{}{
		Record.DataType: record.Type,
		Record.DataId:   record.Id,
	}, record.Map())
	if e != nil {
		return Http.WrapError(e, fmt.Sprintf("failed to apply record [%s/%s]", record.Type, record.Id), http.StatusInternalServerError)
	}
	if isSchema {
		h.SetLocalSchema(record.Id, nil)
		_, ver := Util.ParseCustomPath(record.Id, JsonKey.ArchivedSchemaIdDiv)
		if len(existing) == 0 && ver == "" && h.TypeChangeNotify != nil {
			h.TypeChangeNotify(record.Id)
		}
	}
	return nil
}

// ApplyDelete 删除一条记录（来自 Channel 的 delete 变更）。记录不存在时什么都不做。
func (h *Handler) ApplyDelete(dataType string, dataId string) *Http.HttpError {
	if !IsSyncedRecord(dataType, dataId) {
		return Http.NewHttpError(fmt.Sprintf("record [%s/%s] is internal and cannot be deleted from channel", dataType, dataId), http.StatusBadRequest)
	}
	idKey := fmt.Sprintf("%s/%s", dataType, dataId)
	h.Lock.Aquire(idKey, "HandlerApply")
	defer h.Lock.Release(idKey, "HandlerApply")
	existing, err := h.QueryDb(dataType, dataId)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}
	e := h.DB.Delete(h.Config.DataTable.Data, map[string]interface{}{
		Record.DataType: dataType,
		Record.DataId:   dataId,
	})
	if e != nil {
		return Http.WrapError(e, fmt.Sprintf("failed to delete record [%s/%s]", dataType, dataId), http.StatusInternalServerError)
	}
	if dataType == JsonKey.Schema {
		h.SetLocalSchema(dataId, nil)
		_, ver := Util.ParseCustomPath(dataId, JsonKey.ArchivedSchemaIdDiv)
		if ver == "" && h.TypeChangeNotify != nil {
			h.TypeChangeNotify(dataId)
		}
	}
	return nil
}

// SyncedRecords 返回本 vDS 中所有需要同步的记录：先 schema（含归档版本），再各类型数据。
func (h *Handler) SyncedRecords() ([]map[string]interface{}, *Http.HttpError) {
	schemaList, err := h.QueryDb(JsonKey.Schema, "")
	if err != nil {
		return nil, err
	}
	result := []map[string]interface{}{}
	types := []string{}
	for _, data := range schemaList {
		id, _ := data[Record.DataId].(string)
		if !IsSyncedRecord(JsonKey.Schema, id) {
			continue
		}
		result = append(result, data)
		if _, ver := Util.ParseCustomPath(id, JsonKey.ArchivedSchemaIdDiv); ver == "" {
			types = append(types, id)
		}
	}
	for _, dataType := range types {
		dataList, err := h.QueryDb(dataType, "")
		if err != nil {
			return nil, err
		}
		result = append(result, dataList...)
	}
	return result, nil
}

// ClearSynced 删除本 vDS 中所有需要同步的记录（mirrored vDS 从 snapshot 重建前调用）。
// 先删数据再删 schema；schema 删除不通知 Inventory，重建完成后由 ApplyUpsert 再通知新增的类型。
func (h *Handler) ClearSynced() *Http.HttpError {
	records, err := h.SyncedRecords()
	if err != nil {
		return err
	}
	for idx := len(records) - 1; idx >= 0; idx-- {
		data := records[idx]
		dataType, _ := data[Record.DataType].(string)
		dataId, _ := data[Record.DataId].(string)
		e := h.DB.Delete(h.Config.DataTable.Data, map[string]interface{}{
			Record.DataType: dataType,
			Record.DataId:   dataId,
		})
		if e != nil {
			return Http.WrapError(e, fmt.Sprintf("failed to clear record [%s/%s]", dataType, dataId), http.StatusInternalServerError)
		}
		if dataType == JsonKey.Schema {
			h.SetLocalSchema(dataId, nil)
		}
	}
	return nil
}

// DataTable 返回该 vDS 使用的数据表名。
func (h *Handler) DataTable() string {
	return h.Config.DataTable.Data
}
