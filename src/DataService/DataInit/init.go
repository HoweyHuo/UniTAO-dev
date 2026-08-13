/*
************************************************************************************************************
Copyright (c) 2022 Salesforce, Inc.
All rights reserved.

UniTAO was originally created in 2022 by Shai Herzog & Yi Huo as an
Universal No-Coding Heterogeneous Infrastructure Maintenance & Inventory system that is holistically driven by open/community-developed semantic models/schemas.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>

This copyright notice and license applies to all files in this directory or sub-directories, except when stated otherwise explicitly.
************************************************************************************************************
*/

// Package DataInit provides automatic database initialization for Data Service.
//
// When the config field "initialized" is false (the default when missing),
// InitDatabase creates the data table and imports the base meta-schema. If the
// database already exists and the config field "force-init" is true, the whole
// database is reset (all tables dropped) before recreating the base structure.
package DataInit

import (
	"fmt"
	"log"

	"Data/DbIface"
	"DataService/Config"

	"github.com/salesforce/UniTAO/lib/Schema/Record"
	"github.com/salesforce/UniTAO/lib/Util/Json"
)

const (
	TablesFileMongoDb  = "schema/DataServiceMongoDBTables.json"
	TablesFileDynamoDb = "schema/DataServiceDynamoDBTables.json"
	SchemaFileDefault  = "schema/schema.json"
)

// InitDatabase ensures the database is provisioned with the data table and the
// base meta-schema. It is a no-op when config.Initialized is true.
func InitDatabase(config Config.Confuguration, db DbIface.Database, log *log.Logger) error {
	if config.Initialized {
		return nil
	}
	if config.ForceInit {
		dbExists, err := db.DatabaseExists()
		if err != nil {
			return fmt.Errorf("failed to check database existence. Error:%s", err)
		}
		if dbExists {
			if err := wipeAll(db); err != nil {
				return fmt.Errorf("failed to reset database. Error:%s", err)
			}
			log.Printf("DataInit: database reset by force-init")
		}
	}
	if err := ensureTables(config, db, log); err != nil {
		return fmt.Errorf("failed to create tables. Error:%s", err)
	}
	if err := importMetaSchema(config, db, log); err != nil {
		return fmt.Errorf("failed to import base meta schema. Error:%s", err)
	}
	return nil
}

// wipeAll drops every existing table in the database.
func wipeAll(db DbIface.Database) error {
	tableList, err := db.ListTable()
	if err != nil {
		return err
	}
	for _, tableObj := range tableList {
		tableName, ok := tableObj.(string)
		if !ok {
			continue
		}
		if err := db.DeleteTable(tableName); err != nil {
			return err
		}
	}
	return nil
}

// ensureTables creates the tables defined in the table-meta file if they do not
// already exist. Logical table names in the meta file are translated to the
// physical names configured in "table.data".
func ensureTables(config Config.Confuguration, db DbIface.Database, log *log.Logger) error {
	tableMeta, err := loadTablesMeta(config)
	if err != nil {
		return err
	}
	tableList, err := db.ListTable()
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for _, tableObj := range tableList {
		tableName, ok := tableObj.(string)
		if ok {
			existing[tableName] = true
		}
	}
	for tableName, meta := range tableMeta {
		if existing[tableName] {
			continue
		}
		metaMap := map[string]interface{}{}
		if meta != nil {
			metaMap = meta.(map[string]interface{})
		}
		if err := db.CreateTable(tableName, metaMap); err != nil {
			return err
		}
		log.Printf("DataInit: create table [%s]", tableName)
	}
	return nil
}

// loadTablesMeta reads the table-meta JSON and translates logical table names to
// the configured physical names (e.g. logical "data" -> physical "Data").
func loadTablesMeta(config Config.Confuguration) (map[string]interface{}, error) {
	tableMeta := map[string]interface{}{}
	tablesFile := config.Init.TablesFile
	if tablesFile == "" {
		tablesFile = defaultTablesFile(config.Database.DbType)
	}
	configTables := config.DataTable.Map()
	if tablesFile != "" {
		meta, err := Json.LoadJSONMap(tablesFile)
		if err != nil {
			return nil, err
		}
		for key, metaValue := range meta {
			if customName, ok := configTables[key].(string); ok && customName != "" {
				tableMeta[customName] = metaValue
			} else {
				tableMeta[key] = metaValue
			}
		}
	}
	// always make sure the configured data table is included, even for backends
	// without a table-meta file (e.g. sysdirfile)
	if len(tableMeta) == 0 {
		tableMeta[config.DataTable.Data] = nil
	}
	return tableMeta, nil
}

func defaultTablesFile(dbType string) string {
	switch dbType {
	case "mongodb":
		return TablesFileMongoDb
	case "dynamodb":
		return TablesFileDynamoDb
	}
	return ""
}

// importMetaSchema imports the base meta-schema records from the schema file
// into the data table. Records that already exist are left untouched.
func importMetaSchema(config Config.Confuguration, db DbIface.Database, log *log.Logger) error {
	schemaFile := config.Init.SchemaFile
	if schemaFile == "" {
		schemaFile = SchemaFileDefault
	}
	schemaData, err := Json.LoadJSONMap(schemaFile)
	if err != nil {
		return err
	}
	configTables := config.DataTable.Map()
	for logicalTable, value := range schemaData {
		dataTable := logicalTable
		if customName, ok := configTables[logicalTable].(string); ok && customName != "" {
			dataTable = customName
		}
		if err := importRecords(db, dataTable, value, log); err != nil {
			return err
		}
	}
	return nil
}

// importRecords writes each record in the list if it does not already exist.
func importRecords(db DbIface.Database, dataTable string, records interface{}, log *log.Logger) error {
	recordList, ok := records.([]interface{})
	if !ok {
		return nil
	}
	for _, recObj := range recordList {
		record, ok := recObj.(map[string]interface{})
		if !ok {
			continue
		}
		dataType, _ := record[Record.DataType].(string)
		dataId, _ := record[Record.DataId].(string)
		if dataType == "" || dataId == "" {
			continue
		}
		queryArgs := map[string]interface{}{
			DbIface.Table:    dataTable,
			Record.DataType:  dataType,
			Record.DataId:    dataId,
		}
		exists, err := recordExists(db, queryArgs)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if err := db.Create(dataTable, record); err != nil {
			return fmt.Errorf("failed to create record [%s/%s]. Error:%s", dataType, dataId, err)
		}
		log.Printf("DataInit: import record [%s/%s]", dataType, dataId)
	}
	return nil
}

func recordExists(db DbIface.Database, queryArgs map[string]interface{}) (bool, error) {
	result, err := db.Get(queryArgs)
	if err != nil {
		return false, err
	}
	return len(result) > 0, nil
}
