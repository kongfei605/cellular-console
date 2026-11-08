package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"
)

func (a *App) workspaceReady(w http.ResponseWriter) bool {
	if a.workspace == nil {
		apiError(w, 503, "本地联系人/通话记录不可用，请检查存储；原网络服务独立运行")
		return false
	}
	return true
}
func (a *App) workspaceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/contacts", a.protected(func(w http.ResponseWriter, r *http.Request) {
		if !a.workspaceReady(w) {
			return
		}
		writeJSON(w, 200, a.workspace.Contacts())
	}))
	save := a.protected(func(w http.ResponseWriter, r *http.Request) {
		if !a.workspaceReady(w) {
			return
		}
		var c Contact
		if readJSON(w, r, &c) != nil {
			apiError(w, 400, "联系人格式无效")
			return
		}
		c.ID = r.PathValue("id")
		result, e := a.workspace.PutContact(c)
		if e != nil {
			apiError(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, result)
	})
	mux.HandleFunc("POST /api/contacts", save)
	mux.HandleFunc("POST /api/contacts/{id}", save)
	mux.HandleFunc("GET /api/call-history", a.protected(func(w http.ResponseWriter, r *http.Request) {
		if !a.workspaceReady(w) {
			return
		}
		writeJSON(w, 200, a.workspace.Calls())
	}))
	mux.HandleFunc("POST /api/conversations/{id}/archive", a.protected(func(w http.ResponseWriter, r *http.Request) {
		if !a.workspaceReady(w) {
			return
		}
		id := r.PathValue("id")
		if _, e := conversationFor(a.allMessages(), id); e != nil {
			apiError(w, 404, "会话不存在")
			return
		}
		var in struct {
			Archived bool `json:"archived"`
		}
		if readJSON(w, r, &in) != nil {
			apiError(w, 400, "格式无效")
			return
		}
		if e := a.workspace.Archive(id, in.Archived); e != nil {
			apiError(w, 503, "无法保存回收站状态")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("GET /api/diagnostics", a.protected(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, a.diagnostics()) }))
	mux.HandleFunc("GET /api/diagnostics/export", a.protected(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		f, e := z.Create("diagnostics.json")
		if e == nil {
			e = json.NewEncoder(f).Encode(a.diagnostics())
		}
		if e == nil {
			f, e = z.Create("README.txt")
		}
		if e == nil {
			_, e = fmt.Fprint(f, "Cellular Console — read-only diagnostics\nPhone numbers, SMS bodies, contacts, credentials and modem identifiers are excluded.\nIP addresses, interface counters, firmware and VPN health are included.\nNo commands were executed, no probes were sent, no network settings changed.\nCall history starts when observed; it is not carrier billing data.\n")
		}
		if ce := z.Close(); e == nil {
			e = ce
		}
		if e != nil {
			apiError(w, 500, "诊断导出失败")
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="ec20-diagnostics.zip"`)
		w.Header().Set("Cache-Control", "no-store")
		w.Write(buf.Bytes())
	}))
}
func (a *App) diagnostics() map[string]any {
	s := a.state()
	r := s.Radio
	r.ID = ""
	r.PrimaryPort = ""
	s.Radio = r
	s.Calls = []Call{}
	s.Events = []Event{}
	problem := ""
	if a.workspace == nil {
		problem = "本地工作区不可用"
	} else {
		problem = a.workspace.Error()
	}
	loc := "独立定位页读取NMEA；启用需有线主用且蜂窝备用健康，不自动启用或重试；诊断包不含定位数据"
	if !r.Location {
		loc = "当前 ModemManager 未提供 Location 接口"
	}
	return map[string]any{"generated_at": time.Now().UTC(), "snapshot": s, "runtime": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "workspace_error": problem, "privacy": "不包含号码、短信正文、联系人、设备唯一标识或密码；包含IP地址与网络健康信息", "capabilities": []map[string]string{
		{"name": "设备 / SIM / 信号", "status": "只读状态", "detail": "复用现有 ModemManager 采样，不打开串口"},
		{"name": "网络 / VPN / 流量", "status": "只读状态", "detail": "读取主备控制器状态与接口计数，不执行额外探测"},
		{"name": "浏览器音频回环", "status": "独立服务已验证", "detail": "统一HTTPS入口内的音频页；内部进程隔离，尚未连接EC20通话音频"},
		{"name": "真实通话音频 / UAC", "status": "待现场支持", "detail": "预备代码未启用；需现场确认声卡、音频能力和重启维护窗口"},
		{"name": "定位 / GNSS", "status": "已接入独立页面", "detail": loc},
		{"name": "eSTK 配置管理", "status": "能力未验证", "detail": "尚未确认SIM及配套接口；没有配置文件写入入口"},
		{"name": "VoWiFi / IMS", "status": "未接入", "detail": "需独立核实运营商、认证与ePDG/IMS能力"},
		{"name": "服务代码 / USSD", "status": "未启用", "detail": "不将 *…# 当作普通电话；未增加USSD控制权限"},
		{"name": "AT 终端", "status": "固定只读诊断替代", "detail": "不开放任意AT命令，不抢占PPP或ModemManager串口"},
	}, "note": strings.Join([]string{"此页面不会重启模块、修改USB、路由或APN", "诊断只反映最近一次采样，不代表主动联网测试"}, "；")}
}
