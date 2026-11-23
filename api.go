package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func newUIMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", handleIndex)
	mux.HandleFunc("GET /api/entries", handleList)
	mux.HandleFunc("GET /api/entries/{id}", handleDetail)
	mux.HandleFunc("POST /api/replay", guard(handleReplay))
	mux.HandleFunc("POST /api/clear", guard(handleClear))
	return localOnly(mux)
}

// localOnly отсекает всё, что пришло не с петли — на случай,
// если адрес прослушивания когда-нибудь поменяют на 0.0.0.0.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			http.Error(w, "только с localhost", http.StatusForbidden)
			return
		}
		// Host проверяем и на чтении: через DNS rebinding чужая страница
		// иначе прочитала бы историю с куками и заголовками авторизации.
		if !isLocalHostPort(r.Host) {
			http.Error(w, "недопустимый Host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guard защищает изменяющие ручки от CSRF: случайная страница в браузере
// не должна уметь дёрнуть replay на твоём апстриме.
func guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || !isLocalHostPort(u.Host) {
				http.Error(w, "недопустимый Origin", http.StatusForbidden)
				return
			}
		}
		if !isLocalHostPort(r.Host) {
			http.Error(w, "недопустимый Host", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func isLocalHostPort(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'")
	_, _ = io.WriteString(w, indexHTML)
}

type listItem struct {
	ID         int64  `json:"id"`
	Time       string `json:"time"`
	DurationMs int64  `json:"durationMs"`
	Replay     bool   `json:"replay"`
	Method     string `json:"method"`
	URL        string `json:"url"`
	Status     int    `json:"status"`
	RespSize   int64  `json:"respSize"`
	Err        string `json:"err,omitempty"`
}

func handleList(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	src := store.List(since)
	out := make([]listItem, 0, len(src))
	for _, e := range src {
		out = append(out, listItem{
			ID: e.ID, Time: e.Time.Format("15:04:05"), DurationMs: e.DurationMs,
			Replay: e.Replay, Method: e.Method, URL: e.URL,
			Status: e.Status, RespSize: e.RespSize, Err: e.Err,
		})
	}
	writeJSON(w, out)
}

type bodyView struct {
	Text    string `json:"text"`
	Size    int64  `json:"size"`
	Trunc   bool   `json:"trunc"`
	Skipped bool   `json:"skipped"`
	Binary  bool   `json:"binary"`
	Note    string `json:"note,omitempty"`
}

type detailView struct {
	listItem
	RemoteAddr  string      `json:"remoteAddr"`
	ReqHeaders  [][2]string `json:"reqHeaders"`
	RespHeaders [][2]string `json:"respHeaders"`
	Req         bodyView    `json:"req"`
	Resp        bodyView    `json:"resp"`
}

func handleDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	e := store.Get(id)
	if e == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, detailView{
		listItem: listItem{
			ID: e.ID, Time: e.Time.Format("15:04:05"), DurationMs: e.DurationMs,
			Replay: e.Replay, Method: e.Method, URL: e.URL,
			Status: e.Status, RespSize: e.RespSize, Err: e.Err,
		},
		RemoteAddr:  e.RemoteAddr,
		ReqHeaders:  flatten(e.ReqHeaders),
		RespHeaders: flatten(e.RespHeaders),
		Req:         view(e.ReqBody, e.ReqSize, e.ReqTrunc, e.ReqSkipped, e.ReqHeaders),
		Resp:        view(e.RespBody, e.RespSize, e.RespTrunc, e.RespSkipped, e.RespHeaders),
	})
}

func flatten(h http.Header) [][2]string {
	out := make([][2]string, 0, len(h))
	for k, vs := range h {
		for _, v := range vs {
			out = append(out, [2]string{k, v})
		}
	}
	return out
}

func view(body []byte, size int64, trunc, skipped bool, h http.Header) bodyView {
	bv := bodyView{Size: size, Trunc: trunc, Skipped: skipped}
	if skipped {
		bv.Binary = true
		bv.Note = "бинарное тело (" + h.Get("Content-Type") + ") — не сохраняется"
		return bv
	}
	if size < 0 {
		bv.Note = "тело больше 32 КБ, точный размер не измерялся"
	}
	if len(body) == 0 {
		return bv
	}
	data := body
	switch strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding"))) {
	case "gzip":
		if d, err := ungzip(data); err == nil {
			data = d
		} else if trunc {
			bv.Note = "gzip не распакован: тело обрезано на " + itoa(bodyCap) + " Б"
		} else {
			bv.Note = "gzip не распакован: " + err.Error()
		}
	case "deflate":
		if d, err := io.ReadAll(flate.NewReader(bytes.NewReader(data))); err == nil && len(d) > 0 {
			data = d
		} else {
			bv.Note = "deflate не распакован"
		}
	case "br", "zstd":
		bv.Note = "сжатие не поддерживается для показа — переиграй запрос без Accept-Encoding"
	}
	if !utf8.Valid(data) {
		bv.Binary = true
		bv.Note = "тело не является текстом UTF-8"
		return bv
	}
	bv.Text = string(data)
	return bv
}

func ungzip(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if len(out) > 0 {
		return out, nil // обрезанный gzip всё равно отдаёт начало
	}
	return nil, err
}

func itoa(n int) string { return strconv.Itoa(n) }

// --- replay ---

type replayReq struct {
	Method  string `json:"method"`
	URL     string `json:"url"`
	Headers string `json:"headers"` // сырой блок "Ключ: значение" построчно
	Body    string `json:"body"`
}

func handleReplay(w http.ResponseWriter, r *http.Request) {
	var in replayReq
	if json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in) != nil {
		http.Error(w, "некорректный JSON", http.StatusBadRequest)
		return
	}
	if in.Method == "" {
		in.Method = http.MethodGet
	}
	if !strings.HasPrefix(in.URL, "/") {
		in.URL = "/" + in.URL
	}

	body := []byte(in.Body)
	req, err := http.NewRequest(strings.ToUpper(in.Method), upstream.String()+in.URL,
		bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for _, line := range strings.Split(in.Headers, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "host":
			req.Host = v // Host задаётся полем, не картой заголовков
		case "content-length", "transfer-encoding", "connection", "keep-alive":
			// пересчитывается транспортом: тело могли отредактировать
		default:
			req.Header.Add(k, v)
		}
	}
	// Content-Length выставляем сами по фактическому телу.
	req.ContentLength = int64(len(body))

	e := &Entry{
		Time: time.Now(), Replay: true, Method: req.Method, URL: in.URL,
		RemoteAddr: "replay", ReqHeaders: req.Header.Clone(),
		ReqBody: capBytes(body), ReqSize: int64(len(body)),
		ReqTrunc: len(body) > bodyCap,
	}
	if req.Host != "" {
		e.ReqHeaders.Set("Host", req.Host)
	}

	client := &http.Client{Transport: transport, Timeout: 60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // показываем 3xx как есть
		}}
	resp, err := client.Do(req)
	if err != nil {
		e.Err = err.Error()
		e.Status = http.StatusBadGateway
		e.DurationMs = time.Since(e.Time).Milliseconds()
		store.Add(e)
		writeJSON(w, map[string]any{"id": e.ID, "error": e.Err})
		return
	}
	defer resp.Body.Close()

	e.Status = resp.StatusCode
	e.RespHeaders = resp.Header.Clone()
	skip := !capturable(resp.Header.Get("Content-Type"))
	e.RespSkipped = skip
	n, truncated := drain(resp.Body, skip, &e.RespBody)
	e.RespSize, e.RespTrunc = n, truncated
	e.DurationMs = time.Since(e.Time).Milliseconds()
	store.Add(e)
	writeJSON(w, map[string]any{"id": e.ID, "status": e.Status})
}

func drain(rc io.Reader, skip bool, dst *[]byte) (total int64, trunc bool) {
	buf := make([]byte, 32*1024)
	var kept bytes.Buffer
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			total += int64(n)
			if !skip {
				if rem := bodyCap - kept.Len(); rem >= n {
					kept.Write(buf[:n])
				} else {
					if rem > 0 {
						kept.Write(buf[:rem])
					}
					trunc = true
				}
			}
		}
		if err != nil {
			break
		}
	}
	*dst = kept.Bytes()
	return total, trunc
}

func capBytes(b []byte) []byte {
	if len(b) > bodyCap {
		return b[:bodyCap]
	}
	return b
}

func handleClear(w http.ResponseWriter, r *http.Request) {
	store.Clear()
	w.WriteHeader(http.StatusNoContent)
}
