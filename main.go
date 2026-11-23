// tunnel-inspector — локальный инспектор HTTP-запросов для Cloudflare-туннеля.
//
// Схема: cloudflared -> :4010 (этот прокси) -> приложение на :3000
// UI: http://127.0.0.1:4041 (слушает только loopback, наружу не торчит)
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

var (
	store    *Store
	upstream *url.URL
	uiAddr   string
)

func main() {
	home, _ := os.UserHomeDir()

	listen := flag.String("listen", envOr("TI_LISTEN", "localhost:4010"),
		"адрес прокси — сюда смотрит cloudflared")
	target := flag.String("upstream", envOr("TI_UPSTREAM", "http://localhost:3000"),
		"твоё приложение")
	ui := flag.String("ui", envOr("TI_UI", "localhost:4041"),
		"адрес веб-интерфейса (только loopback)")
	histPath := flag.String("history", envOr("TI_HISTORY",
		filepath.Join(home, ".tunnel-inspector", "history.jsonl")),
		"файл истории")
	flag.Parse()

	// Держим GC агрессивным: инструменту не нужен большой heap.
	debug.SetGCPercent(40)

	// ВНИМАНИЕ: именно присваивание, а не :=. Короткое объявление создало бы
	// локальную upstream и затенило глобальную, которой пользуется прокси, —
	// та осталась бы nil, и каждый запрос падал бы в nil dereference.
	parsed, err := url.Parse(*target)
	if err != nil || parsed.Host == "" {
		log.Fatalf("некорректный -upstream %q", *target)
	}
	upstream = parsed
	uiAddr = *ui

	if err := ensureDir(*histPath); err != nil {
		log.Fatalf("не могу создать каталог истории: %v", err)
	}
	store = NewStore(*histPath)

	stop := make(chan struct{})
	go store.RunPersister(stop)

	proxySrv := &http.Server{
		Handler:           newProxy(),
		ReadHeaderTimeout: 20 * time.Second,
	}
	uiSrv := &http.Server{
		Handler:           newUIMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	proxyLn, err := listenLoopback(*listen)
	if err != nil {
		log.Fatalf("прокси не поднялся на %s: %v", *listen, err)
	}
	uiLn, err := listenLoopback(*ui)
	if err != nil {
		log.Fatalf("UI не поднялся на %s: %v", *ui, err)
	}
	for _, l := range proxyLn {
		go serve(proxySrv, l, "прокси")
	}
	for _, l := range uiLn {
		go serve(uiSrv, l, "UI")
	}

	log.Printf("прокси %s -> %s | UI http://%s", addrsOf(proxyLn), upstream, addrsOf(uiLn))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	close(stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = proxySrv.Shutdown(ctx)
	_ = uiSrv.Shutdown(ctx)

	// Ждём, пока персистер допишет хвост очереди и закроет файл.
	select {
	case <-store.Done():
	case <-time.After(5 * time.Second):
		log.Print("персистер не успел завершиться за 5 с")
	}
	log.Print("остановлен")
}

func serve(s *http.Server, l net.Listener, name string) {
	if err := s.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("%s упал на %s: %v", name, l.Addr(), err)
	}
}

// listenLoopback слушает ОБЕ петли, IPv4 и IPv6, когда в адресе указан
// localhost. cloudflared резолвит localhost в [::1] и на IPv4 не
// переключается, так что односемейная привязка давала бы 502 на ровном месте.
// Явный IP в адресе уважается как есть — наружу всё равно ничего не торчит.
func listenLoopback(addr string) ([]net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	hosts := []string{host}
	if host == "" || host == "localhost" {
		hosts = []string{"127.0.0.1", "::1"}
	}

	var ls []net.Listener
	for _, h := range hosts {
		l, lerr := net.Listen("tcp", net.JoinHostPort(h, port))
		if lerr != nil {
			if len(hosts) > 1 {
				// Одной из семей может не быть — это не повод падать.
				log.Printf("не слушаю %s: %v", net.JoinHostPort(h, port), lerr)
				continue
			}
			err = lerr
			break
		}
		ls = append(ls, l)
	}
	if len(ls) == 0 {
		if err == nil {
			err = errors.New("не удалось занять ни один адрес")
		}
		for _, l := range ls {
			_ = l.Close()
		}
		return nil, err
	}
	return ls, nil
}

func addrsOf(ls []net.Listener) string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Addr().String())
	}
	return strings.Join(out, " + ")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// --- прокси ---

var transport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	MaxIdleConns:          32,
	MaxIdleConnsPerHost:   16,
	IdleConnTimeout:       60 * time.Second,
	ExpectContinueTimeout: time.Second,
	// Не трогаем сжатие: пропускаем Accept-Encoding клиента как есть,
	// распаковываем только для показа в UI.
	DisableCompression: true,
	ForceAttemptHTTP2:  false,
}

func newProxy() http.Handler {
	rp := &httputil.ReverseProxy{
		Transport: &captureTransport{next: transport},
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = upstream.Scheme
			r.Out.URL.Host = upstream.Host
			// ReverseProxy «подчищает» query и выбрасывает параметры
			// с точкой с запятой (?a=1;b=2&c=3 доходило как ?c=3).
			// Инспектор обязан пропускать строку запроса как есть.
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			// Host оставляем клиентский: приложение должно видеть
			// настоящее имя из туннеля, а не 127.0.0.1.
			r.Out.Host = r.In.Host

			// ReverseProxy в режиме Rewrite вырезает входящие X-Forwarded-*
			// и Forwarded, чтобы внешний клиент не мог их подделать. Но наш
			// единственный клиент — доверенный локальный cloudflared, и именно
			// он несёт реальный IP посетителя и X-Forwarded-Proto: https.
			// Если их потерять, приложение будет строить http-редиректы и
			// ронять secure-куки и OAuth-коллбэки. Возвращаем как есть.
			for _, h := range []string{"X-Forwarded-For", "X-Forwarded-Proto",
				"X-Forwarded-Host", "X-Forwarded-Port", "Forwarded",
				"Cf-Connecting-Ip", "Cf-Ray", "Cf-Ipcountry", "Cf-Visitor"} {
				if v, ok := r.In.Header[h]; ok {
					r.Out.Header[h] = v
				}
			}
			// Своего ничего не добавляем: приложение должно видеть ровно то,
			// что видело бы без инспектора в цепи. Выдуманный
			// X-Forwarded-Proto: http умеет ронять приложение в цикл
			// редиректов, если cloudflared этот заголовок не прислал.
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Приложение не отвечает — отдаём 502, но запрос всё равно
			// остаётся в истории, чтобы его можно было переиграть позже.
			if e, ok := r.Context().Value(entryKey{}).(*Entry); ok {
				e.Err = err.Error()
				e.Status = http.StatusBadGateway
				e.DurationMs = time.Since(e.Time).Milliseconds()
				store.Add(e)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("tunnel-inspector: апстрим " + upstream.String() +
				" недоступен\n" + err.Error() + "\n"))
		},
	}
	return rp
}

type entryKey struct{}

type captureTransport struct{ next http.RoundTripper }

func (t *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	e := &Entry{
		Time:       time.Now(),
		Method:     r.Method,
		URL:        r.URL.RequestURI(),
		RemoteAddr: r.Header.Get("X-Forwarded-For"),
		ReqHeaders: r.Header.Clone(),
	}
	e.ReqHeaders.Set("Host", r.Host)

	// Тело запроса снимаем синхронно, ДО обращения к апстриму. Ленивый tee
	// здесь не годится: при отказе соединения транспорт закрывает тело
	// непрочитанным, и вебхук, пришедший на лежащее приложение, терялся бы —
	// а это как раз главный сценарий для replay. Побочно уходит гонка:
	// все поля запроса заполнены до store.Add.
	if r.Body != nil && r.Body != http.NoBody {
		skip := !capturable(r.Header.Get("Content-Type"))
		e.ReqSkipped = skip
		head, body, size, trunc := prereadBody(r.Body, skip, r.ContentLength)
		e.ReqBody, e.ReqSize, e.ReqTrunc = head, size, trunc
		r.Body = body
	}

	// Пробрасываем запись в ErrorHandler через контекст.
	*r = *r.WithContext(context.WithValue(r.Context(), entryKey{}, e))

	resp, err := t.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}

	e.Status = resp.StatusCode
	e.RespHeaders = resp.Header.Clone()

	// 101 Switching Protocols: ReverseProxy берёт тело как io.ReadWriteCloser,
	// чтобы прокинуть сырой WebSocket-поток в обе стороны. Обёртка его сломает,
	// поэтому тело не трогаем — пишем только сам факт апгрейда.
	if resp.StatusCode == http.StatusSwitchingProtocols ||
		resp.Body == nil || resp.Body == http.NoBody {
		e.DurationMs = time.Since(e.Time).Milliseconds()
		store.Add(e)
		return resp, nil
	}

	skip := !capturable(resp.Header.Get("Content-Type"))
	e.RespSkipped = skip
	resp.Body = newCapReader(resp.Body, skip, func(c *capReader) {
		e.RespBody = c.buf.Bytes()
		e.RespSize = c.total
		e.RespTrunc = c.trunc
		e.DurationMs = time.Since(e.Time).Milliseconds()
		store.Add(e)
	})
	return resp, nil
}
