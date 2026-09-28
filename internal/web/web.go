// Package web 提供最小的查看界面与 JSON API。
//
// 刻意不引入任何前端构建链（无 npm/vite）：模板 + 一点原生 JS，
// 单二进制直接带着页面走。
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"acs/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Refresher 是「让设备重新上报基本信息」的能力，由 cwmp.Server 实现。
// 用接口是为了避免 web 包反向依赖 cwmp 包。
type Refresher interface {
	RequestRefresh(deviceID int64) error
}

// Server 是界面服务。
type Server struct {
	store     *store.Store
	refresher Refresher
	tpl       *template.Template
}

// kv 是详情页里的一行「字段 - 值」。
type kv struct {
	K string
	V string
}

// Register 把界面路由挂到 mux 上。
func Register(mux *http.ServeMux, st *store.Store, ref Refresher) error {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"fmtTime": formatTime,
		"uptime":  formatUptime,
	}).ParseFS(assets, "templates/*.html")
	if err != nil {
		return fmt.Errorf("解析模板失败: %w", err)
	}

	s := &Server{store: st, refresher: ref, tpl: tpl}

	sub, err := fs.Sub(assets, "static")
	if err != nil {
		return err
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(sub)))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /devices/{id}", s.handleDevice)
	mux.HandleFunc("POST /devices/{id}/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/devices", s.apiDevices)
	mux.HandleFunc("GET /api/devices/{id}", s.apiDevice)
	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices()
	if err != nil {
		http.Error(w, "读取设备列表失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	stats, err := s.store.Stats()
	if err != nil {
		http.Error(w, "读取统计失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := map[string]any{
		"Devices": devices,
		"Stats":   stats,
		"Path":    r.URL.Path,
	}
	if err := s.tpl.ExecuteTemplate(w, "index.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.store.GetDevice(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	params, err := s.store.ListParams(id)
	if err != nil {
		http.Error(w, "读取参数失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tasks, err := s.store.ListTasks(id, 30)
	if err != nil {
		http.Error(w, "读取任务失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	informs, err := s.store.ListInforms(id, 20)
	if err != nil {
		http.Error(w, "读取上报记录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	pending, _ := s.store.PendingTaskCount(id)

	data := map[string]any{
		"Device":  d,
		"Basic":   basicInfo(d, params),
		"Params":  params,
		"Tasks":   tasks,
		"Informs": informs,
		"Pending": pending,
		"Path":    "/devices/" + strconv.FormatInt(id, 10),
	}
	if err := s.tpl.ExecuteTemplate(w, "device.html", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.refresher != nil {
		_ = s.refresher.RequestRefresh(id)
	}
	http.Redirect(w, r, "/devices/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) apiDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices()
	if err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"data": devices, "count": len(devices)})
}

func (s *Server) apiDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSONError(w, fmt.Errorf("设备 ID 非法"), http.StatusBadRequest)
		return
	}
	d, err := s.store.GetDevice(id)
	if err != nil {
		writeJSONError(w, fmt.Errorf("设备不存在"), http.StatusNotFound)
		return
	}
	params, _ := s.store.ListParams(id)
	tasks, _ := s.store.ListTasks(id, 50)
	pending, _ := s.store.PendingTaskCount(id)
	informs, _ := s.store.ListInforms(id, 20)
	writeJSON(w, map[string]any{
		"device":        d,
		"params":        params,
		"tasks":         tasks,
		"pending_tasks": pending,
		"informs":       informs,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// basicInfo 组装详情页顶部「最基本的设备信息」。
// 数据模型根未知时不做猜测，直接按已有参数原样展示。
func basicInfo(d *store.Device, params []store.Param) []kv {
	idx := make(map[string]string, len(params))
	for _, p := range params {
		idx[p.Name] = p.Value
	}
	get := func(names ...string) string {
		for _, n := range names {
			if v, ok := idx[n]; ok && v != "" {
				return v
			}
		}
		return ""
	}

	root := d.DataModelRoot
	if root == "" {
		root = "InternetGatewayDevice."
	}

	out := []kv{
		{"厂商", d.Manufacturer},
		{"型号", d.ModelName},
		{"序列号", d.SerialNumber},
		{"OUI", d.OUI},
		{"ProductClass", d.ProductClass},
		{"软件版本", d.SoftwareVersion},
		{"硬件版本", d.HardwareVersion},
		{"Spec 版本", d.SpecVersion},
		{"ProvisioningCode", d.ProvisioningCode},
		{"运行时长", formatUptime(get(root + "DeviceInfo.UpTime"))},
		{"外网 IP", d.ExternalIP},
		{"上报周期", intervalText(d.PeriodicInterval)},
		{"ConnectionRequestURL", d.ConnRequestURL},
		{"数据模型根", d.DataModelRoot},
		{"最近事件", d.LastEvents},
		{"最后上报", formatTime(d.LastInformAt)},
		{"最后启动", formatTime(d.LastBootAt)},
		{"来源 IP", d.SourceIP},
		{"User-Agent", d.UserAgent},
	}

	// 空值不展示，避免一屏的 "-"
	kept := out[:0]
	for _, x := range out {
		if strings.TrimSpace(x.V) != "" && x.V != "-" {
			kept = append(kept, x)
		}
	}
	return kept
}

func intervalText(sec int) string {
	if sec <= 0 {
		return ""
	}
	return (time.Duration(sec) * time.Second).String()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// formatUptime 把 TR-069 的秒数格式化成人看的。
// 直接接受字符串，因为 CPE 上报的 UpTime 就是字符串。
func formatUptime(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	sec, err := strconv.ParseInt(v, 10, 64)
	if err != nil || sec < 0 {
		return v
	}
	d := sec / 86400
	h := (sec % 86400) / 3600
	m := (sec % 3600) / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%d 天 %d 小时 %d 分", d, h, m)
	case h > 0:
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	default:
		return fmt.Sprintf("%d 分 %d 秒", m, sec%60)
	}
}
