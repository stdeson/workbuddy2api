package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestResourcePackagesAllPackages：不带 ProductCode/Status 过滤，逐套餐返回
// used/total/remaining/resetAt；周期包优先 CycleCapacity 三字段（packageRemainUsed 口径）。
func TestResourcePackagesAllPackages(t *testing.T) {
	var gotBody string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[`+
			`{"PackageName":"周期包","CycleEndTime":"2026-10-01 00:00:00","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200},`+
			`{"PackageName":"","CapacitySize":100,"CapacityRemain":40,"CapacityUsed":60}`+
			`]}}}}`), nil
	})
	pkgs, err := c.ResourcePackages(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("ResourcePackages: %v", err)
	}
	if strings.Contains(gotBody, "ProductCode") || strings.Contains(gotBody, "Status") {
		t.Errorf("请求体不应带 ProductCode/Status 过滤（会隐藏赠送包）: %s", gotBody)
	}
	if len(pkgs) != 2 {
		t.Fatalf("packages=%d want 2 (%+v)", len(pkgs), pkgs)
	}
	if pkgs[0].PackageName != "周期包" || pkgs[0].CycleCapacitySize != 500 ||
		pkgs[0].CycleCapacityUsed != 200 || pkgs[0].CycleCapacityRemain != 300 {
		t.Errorf("周期包字段错: %+v", pkgs[0])
	}
	if !pkgs[0].Recurring {
		t.Error("周期包 Recurring 应为 true")
	}
	if pkgs[0].CycleEndTime != "2026-10-01 00:00:00" {
		t.Errorf("resetAt=%q", pkgs[0].CycleEndTime)
	}
	// 无名包回落占位名；非周期包 Recurring=false，size/used 走 Capacity 三字段。
	if pkgs[1].PackageName != "Credit Package" || pkgs[1].Recurring {
		t.Errorf("无名非周期包: %+v", pkgs[1])
	}
	if pkgs[1].CycleCapacitySize != 100 || pkgs[1].CycleCapacityUsed != 60 || pkgs[1].CycleCapacityRemain != 40 {
		t.Errorf("非周期包字段错: %+v", pkgs[1])
	}
}

// TestResourcePackagesUpstreamError：上游业务码非 0 → 返回错误（不吞成空列表，
// 看板据此显示 error 而不是"没有套餐"）。
func TestResourcePackagesUpstreamError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":11101,"msg":"bad params"}`), nil
	})
	if _, err := c.ResourcePackages(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("业务码非 0 应报错")
	}
}
