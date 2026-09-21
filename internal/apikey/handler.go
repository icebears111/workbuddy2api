package apikey

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Handler 暴露 key 管理的 HTTP 接口。
//
// 路由（由各桥的 mux 注册在 /admin/api/keys 下）：
//   GET    /admin/api/keys        列出全部 key（key 本身**脱敏**返回）
//   POST   /admin/api/keys        新建，body {"name":"...","note":"..."}
//   DELETE /admin/api/keys/{id}   吊销
//
// ── 为什么列表默认脱敏 ──────────────────────────────────────
// 明文 key 一旦离开服务器就再也收不回来。看板需要"复制完整 key"的能力，
// 走 GET /admin/api/keys/{id}/reveal 单一端点，这样：
//   · 常规刷新不会把明文带进浏览器缓存 / 日志 / 截图；
//   · 需要明文时必须显式点一次"显示"，行为可审计。
//
// ── 鉴权 ────────────────────────────────────────────────────
// 本包不自己做鉴权 —— 由调用方把 Handler 包在桥既有的管理鉴权中间件里
// （三座桥的 /admin 路由都要求 API key）。这样不引入第二套鉴权逻辑。
type Handler struct {
	store *Store
	// BasePath 路由前缀，用于从 URL 里剥出 {id}。
	// 默认 "/admin/api/keys"；调用方挂在别处时须同步设置。
	BasePath string
	// NewKey, DeleteKey 由调用方注入的副作用钩子，可为 nil。
	// 例如 codebuddy2api 可以在新建 key 后立刻写一条日志。
	OnCreate func(*Key)
	OnDelete func(string)
}

// NewHandler 构造。BasePath 默认 "/admin/api/keys"。
func NewHandler(store *Store) *Handler {
	return &Handler{store: store, BasePath: "/admin/api/keys"}
}

// idFrom 从请求路径里剥出 {id} 与动作段（如 "reveal"）。
func (h *Handler) idFrom(path string) (id, action string) {
	base := h.BasePath
	if base == "" {
		base = "/admin/api/keys"
	}
	rest := strings.Trim(strings.TrimPrefix(path, base), "/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return rest, ""
}

// Register 在 mux 上注册路由。prefix 形如 "/admin/api/keys"。
//
// 注意：若调用方的 mux 上还有更宽的 catch-all（如 "GET /admin/"），
// 用它注册会触发 Go 1.22+ ServeMux 的冲突 panic。那种场景改用导出的
// List / Create / Get / Delete 方法自行注册（路径要带方法）。
func (h *Handler) Register(mux *http.ServeMux, prefix string) {
	prefix = strings.TrimSuffix(prefix, "/")
	mux.HandleFunc("GET "+prefix, h.List)
	mux.HandleFunc("POST "+prefix, h.Create)
	mux.HandleFunc("DELETE "+prefix+"/", h.Delete)
	mux.HandleFunc("GET "+prefix+"/", h.Get) // GET /{id} 与 /{id}/reveal
}

// dto 对外形状：不含明文。
type dto struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Note      string `json:"note,omitempty"`
	Masked    string `json:"masked"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used,omitempty"`
}

func toDTO(k *Key) dto {
	d := dto{
		ID:        k.ID,
		Name:      k.Name,
		Note:      k.Note,
		Masked:    k.MaskedSecret(),
		CreatedAt: k.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if !k.LastUsed.IsZero() {
		d.LastUsed = k.LastUsed.Format("2006-01-02T15:04:05Z07:00")
	}
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error"},
	})
}

func (h *Handler) List(w http.ResponseWriter, _ *http.Request) {
	keys := h.store.List()
	out := make([]dto, 0, len(keys))
	for _, k := range keys {
		out = append(out, toDTO(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out, "count": len(out)})
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	k, err := h.store.Create(req.Name, req.Note)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.OnCreate != nil {
		h.OnCreate(k)
	}
	// 新建时**返回一次明文** —— 这是用户唯一一次能看到它。
	// 后续只能通过 /{id}/reveal 再看（需要显式动作）。
	resp := map[string]any{}
	b, _ := json.Marshal(toDTO(k))
	_ = json.Unmarshal(b, &resp)
	resp["key"] = k.Key
	resp["notice"] = "这是完整密钥，请立即保存；后续可在列表里点「显示」查看。"
	writeJSON(w, http.StatusCreated, resp)
}

// get 处理 GET /{id} 与 GET /{id}/reveal。
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, action := h.idFrom(r.URL.Path)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 key id")
		return
	}

	var found *Key
	for _, k := range h.store.List() {
		if k.ID == id {
			found = k
			break
		}
	}
	if found == nil {
		writeErr(w, http.StatusNotFound, "key 不存在")
		return
	}

	if action == "reveal" {
		resp := map[string]any{}
		b, _ := json.Marshal(toDTO(found))
		_ = json.Unmarshal(b, &resp)
		resp["key"] = found.Key
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if action != "" {
		writeErr(w, http.StatusNotFound, "未知操作")
		return
	}
	writeJSON(w, http.StatusOK, toDTO(found))
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, _ := h.idFrom(r.URL.Path)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 key id")
		return
	}
	if err := h.store.Delete(id); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if h.OnDelete != nil {
		h.OnDelete(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
