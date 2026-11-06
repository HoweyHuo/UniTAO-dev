package VdsTest

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/salesforce/UniTAO/lib/Util/Http"
)

// rawPatch 原样发送请求体，不做 JSON 编码。
func rawPatch(t *testing.T, url string, body string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s failed: %s", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return response{Status: resp.StatusCode, Body: raw}
}

// PATCH 字符串属性：JSON 字符串存去掉引号后的值，非 JSON 的原文本保持兼容。
func TestPatchStringValue(t *testing.T) {
	ds := newDsNode(t, "dsA", "")
	ds.start(t, map[string]interface{}{"local": map[string]interface{}{}})
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", schemaRecord("rack", "0.0.1", map[string]interface{}{
		"name":  map[string]interface{}{"type": "string"},
		"units": map[string]interface{}{"type": "integer"},
	})), http.StatusCreated, "post rack schema")
	expectStatus(t, call(t, http.MethodPost, ds.Url+"/local", dataRecord("rack", "r1", map[string]interface{}{
		"name": "rack one", "units": 42,
	})), http.StatusCreated, "post r1")

	nameUrl := ds.Url + "/local/rack/r1/name"
	cases := []struct {
		body   string
		expect string
	}{
		{`"rack one v2"`, "rack one v2"}, // JSON 字符串
		{`"123"`, "123"},                 // 像数字的字符串仍是字符串
		{`rack one v3`, "rack one v3"},   // 旧客户端发的原文本
	}
	for _, c := range cases {
		expectStatus(t, rawPatch(t, nameUrl, c.body), http.StatusAccepted, "patch name "+c.body)
		data := call(t, http.MethodGet, ds.Url+"/local/rack/r1", nil).Map(t)["data"].(map[string]interface{})
		if data["name"] != c.expect {
			t.Fatalf("patch name with body %s: expect %q, got %#v", c.body, c.expect, data["name"])
		}
	}

	expectStatus(t, rawPatch(t, ds.Url+"/local/rack/r1/units", `48`), http.StatusAccepted, "patch units")
	data := call(t, http.MethodGet, ds.Url+"/local/rack/r1", nil).Map(t)["data"].(map[string]interface{})
	if data["units"] != float64(48) {
		t.Fatalf("patch units: expect 48, got %#v", data["units"])
	}
}

// SubmitPayload 发出的任意值经 LoadRequest 都能还原（跨 DS PATCH 走这条路径）。
func TestSubmitPayloadRoundTrip(t *testing.T) {
	var got interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, e := Http.LoadRequest(r)
		if e != nil {
			t.Errorf("LoadRequest failed: %s", e)
		}
		got = data
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	values := []interface{}{"vm-01", "123", "true", "{name}", float64(7), true, []interface{}{"a"}, map[string]interface{}{"k": "v"}}
	for _, value := range values {
		resp, _, err := Http.SubmitPayload(server.URL, http.MethodPatch, nil, value)
		if err != nil {
			t.Fatalf("SubmitPayload(%#v) failed: %s", value, err)
		}
		resp.Body.Close()
		if !reflect.DeepEqual(got, value) {
			t.Fatalf("SubmitPayload(%#v): server got %#v", value, got)
		}
	}
}
