package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newCheckinTestProvider 构造指向假上游的 provider。
func newCheckinTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient()
	c.SetBases(srv.URL, srv.URL)
	return NewProvider(c, Credential{AccessToken: "t", UID: "uid-test"})
}

// TestCheckinAlreadyWithBody 是步骤⑦真实回归抓到的形态：
// HTTP 400 + JSON body {code:10001, msg:"今天已签到，请明天再来"}。
// 必须判为幂等成功，而不是错误。
func TestCheckinAlreadyWithBody(t *testing.T) {
	p := newCheckinTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到，请明天再来","requestId":"x"}`))
	})

	res, err := p.Checkin(context.Background(), "")
	if err != nil {
		t.Fatalf("已签到(HTTP400+body)不应返回错误，实际: %v", err)
	}
	if !res.AlreadyCheckedIn {
		t.Errorf("应标记 AlreadyCheckedIn，实际 %+v", res)
	}
}

// TestCheckinAlreadyEmptyBody 是 2026-10-04 观测的形态：
// HTTP 400 + 空 body。同样必须幂等成功。
func TestCheckinAlreadyEmptyBody(t *testing.T) {
	p := newCheckinTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	res, err := p.Checkin(context.Background(), "")
	if err != nil {
		t.Fatalf("已签到(HTTP400+空body)不应返回错误: %v", err)
	}
	if !res.AlreadyCheckedIn {
		t.Errorf("应标记 AlreadyCheckedIn，实际 %+v", res)
	}
}

// TestCheckinRealFailureIsError 验证真正的失败不被吞掉。
func TestCheckinRealFailureIsError(t *testing.T) {
	p := newCheckinTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":50001,"msg":"参数错误"}`))
	})
	if _, err := p.Checkin(context.Background(), ""); err == nil {
		t.Fatal("业务失败应返回错误")
	}
}

// TestCheckinSuccess 验证首次签到成功（200 + code=0）。
func TestCheckinSuccess(t *testing.T) {
	p := newCheckinTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	})
	res, err := p.Checkin(context.Background(), "")
	if err != nil {
		t.Fatalf("签到成功不应返回错误: %v", err)
	}
	if res.AlreadyCheckedIn {
		t.Error("首签不应标记 AlreadyCheckedIn")
	}
}
