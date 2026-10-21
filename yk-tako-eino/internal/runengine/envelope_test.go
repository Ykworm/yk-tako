package runengine

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestInternalEnvelope 钉住 500 出口的信封字节（doc/40 A7-8 收口补的）。
//
// internal() 是全仓 76 处调用的**公共 500 出口**：存储 / ID 生成类故障都走它。
// 换信封之前它回 {"error":"internal"}，换之后回业务码信封——但**改动前后都没有单测断过它的 body**，
// 这里补上：字节逐字断（code → message → data 的顺序也是契约），且原始错误串不得漏进响应体
// （原因只进服务端日志）。
func TestInternalEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/runs", nil)
	internal(c, errors.New("存储炸了：dsn=postgres://user:pw@host/db"))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d, want 500", w.Code)
	}
	const want = `{"code":111901,"message":"服务暂时不可用，请再试一次","data":null}`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Fatalf("500 体 = %s\nwant %s", got, want)
	}
	// 机器细节不进响应体（原始错误串、DSN 都不该漏出去）。
	if strings.Contains(w.Body.String(), "存储炸了") || strings.Contains(w.Body.String(), "dsn=") {
		t.Fatalf("响应体泄露了原始错误：%s", w.Body.String())
	}
}
