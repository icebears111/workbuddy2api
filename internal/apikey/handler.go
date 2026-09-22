package apikey

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Handler 暴露 key 管理的 HTTP 接口。
//
// 路由（由各桥的 mux 注册在 /admin/api/keys 下）：
//
//	GET    /admin/api/keys        列出全部 key（key 本身**脱敏**返回）
//	POST   /admin/api/keys        新建，body {"name":"...","note":"..."}
//	DELETE /admin/api/keys/{id}   吊销
//
// ── 为什么列表默认脱敏 ──────────────────────────────────────
// 明文 key 一旦离开服务器就再也收不回来。看板需要"复制完整 key"的能力，
// 走 GET /admin/api/keys/{id}/reveal 单一端点，这样：
//
//	· 常规刷新不会把明文带进浏览器缓存 / 日志 / 截图；
//	· 需要明文时必须显式点一次"显示"，行为可审计。
//
// ── 鉴权 ────────────────────────────────────────────────────
// 本包不自己做鉴权 —— 由调用方把 Handler 包在桥既有的管理鉴权中间件里
// （三座桥的 /admin 路由都要求 API key）。这样不引入第二套鉴权逻辑。
//
// ── 归属与「管理员能看到什么」（2026-09-22 收紧）──────────────
// 起因：站长发现成员的 key（88，owner=2260185529）出现在自己的
// 「API Key」页里，且长得和自己那把一模一样 —— 因为原来管理员
// `owner` 被置空 → List 不过滤 → 看全部，且 DTO 里**没有 owner 字段**，
// 界面上根本分不清哪把是谁的。更糟的是管理员还能 reveal 出成员 key 的**明文**、
// 还能吊销。
//
// 「发出去的凭证」如果管理员随时能取回明文，那它对成员就不是真正的凭证 ——
// 成员换不走、吊销不掉，且一旦管理员账号被冒用，全部成员的 key 一起泄漏。
//
// 现在的规则（三条，按请求方的身份分）：
//
//	① 成员自己的 key      → 看得到明文、可吊销
//	② 管理员自己名下的 key → 看得到明文、可吊销（owner=="" 即管理员/通用级）
//	③ 管理员看成员的 key   → **只读**：列表里能看到（含归属列，便于审计
//	   「谁领了 key」），但**不能 reveal、不能吊销**
//
// 第 ③ 条是有意保留的可见性：管理员需要知道「谁在用网关」，否则出了问题
// 无从追查；但他不该能拿走或销毁别人的凭证。这两件事必须分开。
type Handler struct {
	store *Store
	// BasePath 路由前缀，用于从 URL 里剥出 {id}。
	// 默认 "/admin/api/keys"；调用方挂在别处时须同步设置。
	BasePath string
	// NewKey, DeleteKey 由调用方注入的副作用钩子，可为 nil。
	// 例如 codebuddy2api 可以在新建 key 后立刻写一条日志。
	OnCreate func(*Key)
	OnDelete func(string)
	// OwnerFrom 由调用方注入：从请求解析 (owner, admin)。
	// owner=="" 且 admin=true → 管理员；owner!="" → 只看/管自己的。
	//
	// nil 时按管理员处理（兼容未接身份的调用方）——**但那种部署下
	// 上面第 ③ 条的隔离不会生效**，因为拿不出「请求方是谁」。
	// 三座桥都已接线，新接入的桥务必一并设置。
	OwnerFrom func(r *http.Request) (owner string, admin bool)
}

// scope 解析当前请求的作用域。
func (h *Handler) scope(r *http.Request) (owner string, admin bool) {
	if h.OwnerFrom == nil {
		return "", true
	}
	return h.OwnerFrom(r)
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
	// Owner 归属（SSO 用户名）。空 = 管理员/通用级。
	//
	// 2026-09-22 补：原来 DTO 里没有这个字段，于是管理员的列表里
	// 成员的 key 和自己的 key 长得完全一样，界面上无从分辨
	// （用户的反馈原话是「怎么到我管理员手上了」）。
	// 归属是**审计需要**的信息，本就该显示；它不泄露 key 本身。
	Owner string `json:"owner,omitempty"`
	// Editable 调用方是否有权改这把 key（reveal 明文 / 吊销）。
	// 管理员看成员的 key 时是 false —— 界面据此把按钮换成说明，
	// 而不是让用户点了才收到 403。
	Editable bool `json:"editable"`
}

// toDTO 把存储记录转成对外形状。viewer 决定 editable：
//   - viewer==""（管理员）时，只有无主（管理员级）的 key 可编辑
//   - viewer!=""（成员）时，只有自己名下的可编辑
//
// 为什么不在这里直接过滤掉不可编辑的：管理员**需要看到**成员领了哪些
// key（审计），但不需要能改它们。过滤掉会让「谁在用」这件事不可查。
func toDTO(k *Key, viewer string) dto {
	d := dto{
		ID:        k.ID,
		Name:      k.Name,
		Note:      k.Note,
		Masked:    k.MaskedSecret(),
		CreatedAt: k.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		Owner:     k.Owner,
		Editable:  k.Owner == viewer,
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

// List 列出 key。
//
//	成员             → 只有自己的
//	管理员           → 全部（含各成员的），带 owner 列供审计
//	                   但对成员的那些，editable=false（不能 reveal/吊销）
//
// 「管理员看全部」是有意保留的：他需要知道谁在用网关。但他拿不走、
// 也销不掉别人的凭证 —— 见 toDTO 与 Get/Delete 的判定。
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	viewer, admin := h.scope(r)
	if admin {
		viewer = ""
	}
	keys := h.store.List(viewer) // viewer!="" 时只回自己的
	out := make([]dto, 0, len(keys))
	for _, k := range keys {
		out = append(out, toDTO(k, viewer))
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
	owner, admin := h.scope(r)
	if admin {
		owner = "" // 管理员创建的 key 为管理员/通用级
	}
	k, err := h.store.Create(req.Name, req.Note, owner)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.OnCreate != nil {
		h.OnCreate(k)
	}
	// 新建时**返回一次明文** —— 这是用户唯一一次能看到它。
	// 后续只能通过 /{id}/reveal 再看（需要显式动作）。
	// viewer 传 owner：刚建的这把必定归自己，所以 editable=true。
	resp := map[string]any{}
	b, _ := json.Marshal(toDTO(k, owner))
	_ = json.Unmarshal(b, &resp)
	resp["key"] = k.Key
	resp["notice"] = "这是完整密钥，请立即保存；后续可在列表里点「显示」查看。"
	writeJSON(w, http.StatusCreated, resp)
}

// get 处理 GET /{id} 与 GET /{id}/reveal。
//
// 权限（2026-09-22 收紧）：
//
//	成员 → 只能取自己名下的 key
//	管理员 → 能取到**任何** key 的记录（为了审计），
//	        但只有无主（管理员级）的能 reveal 出明文
//
// 关键的一条：**管理员不能 reveal 成员的 key**。
// 原来那种「管理员万能」的写法（owner 置空 → 全部放行）会让成员的凭证
// 对管理员透明 —— 成员换不掉、也说不清谁用过；而管理员账号一旦被冒用，
// 全部成员的 key 一起泄漏。可见性（知道有这把 key）与可控性（拿走它）
// 必须分开：前者管理员需要，后者他不需要。
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, action := h.idFrom(r.URL.Path)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 key id")
		return
	}

	viewer, admin := h.scope(r)
	if admin {
		viewer = ""
	}
	// 取记录。**注意 store.Get 的语义**：owner=="" 表示「不过滤」，
	// 所以管理员（viewer==""）一次调用就能取到任何 key ——
	// 不能靠「第一次取不到再放开取一次」来判定归属，
	// 那个分支对管理员永远不会走到（这个坑我第一版就踩了）。
	found, ok := h.store.Get(id, viewer)
	if !ok {
		writeErr(w, http.StatusNotFound, "key 不存在")
		return
	}
	// 管理员拿到的是**别人的** key → 只能看，不能动。
	// 判定用 found.Owner != viewer：admin 的 viewer 恒为 ""，
	// 于是「有主的 key」一律 inspectOnly。
	inspectOnly := admin && found.Owner != ""

	if action == "reveal" {
		if inspectOnly {
			// 不是 404：这把 key 确实存在（列表里能看到），
			// 只是不该由管理员取明文。404 会让人以为 key 没了。
			writeErr(w, http.StatusForbidden,
				"该 key 属于其他用户（"+found.Owner+"），管理员可以查看它的存在与使用情况，但不能取出明文")
			return
		}
		resp := map[string]any{}
		b, _ := json.Marshal(toDTO(found, viewer))
		_ = json.Unmarshal(b, &resp)
		resp["key"] = found.Key
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if action != "" {
		writeErr(w, http.StatusNotFound, "未知操作")
		return
	}
	writeJSON(w, http.StatusOK, toDTO(found, viewer))
}

// Delete 吊销一个 key。
//
// 与 Get 同一条规则：**管理员不能吊销成员的 key**。
// 这与「管理员能停用成员的账号」不同 —— 账号停用是可逆的运维动作，
// 而吊销 key 是销毁凭证：成员的客户端会立刻 401，而他不知道是谁干的。
// 要收回某人的访问权，正确做法是让成员自己吊销、或停用他的账号
// （后者看板有开关，且可逆）。
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, _ := h.idFrom(r.URL.Path)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 key id")
		return
	}
	viewer, admin := h.scope(r)
	if admin {
		viewer = ""
	}
	// 管理员先探一下这把 key 是不是别人的：是则拒绝（并给出可操作的说明），
	// 而不是让 store.Delete 用「不存在」把真实原因盖掉。
	if admin {
		if k, ok := h.store.Get(id, ""); ok && k.Owner != "" {
			writeErr(w, http.StatusForbidden,
				"该 key 属于其他用户（"+k.Owner+"），管理员不能代其吊销。"+
					"如需收回访问权，请停用该成员的账号（可逆），或让本人自行吊销")
			return
		}
	}
	if err := h.store.Delete(id, viewer); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if h.OnDelete != nil {
		h.OnDelete(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
