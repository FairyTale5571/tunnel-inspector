package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	bodyCap    = 32 * 1024 // максимум байт тела, которые храним
	maxEntries = 100       // размер кольцевого буфера
)

// Entry — одна записанная пара запрос/ответ.
type Entry struct {
	ID          int64       `json:"id"`
	Time        time.Time   `json:"time"`
	DurationMs  int64       `json:"durationMs"`
	Replay      bool        `json:"replay"`
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	RemoteAddr  string      `json:"remoteAddr"`
	ReqHeaders  http.Header `json:"reqHeaders"`
	ReqBody     []byte      `json:"reqBody,omitempty"`
	ReqSize     int64       `json:"reqSize"`
	ReqTrunc    bool        `json:"reqTrunc"`
	ReqSkipped  bool        `json:"reqSkipped"`
	Status      int         `json:"status"`
	RespHeaders http.Header `json:"respHeaders"`
	RespBody    []byte      `json:"respBody,omitempty"`
	RespSize    int64       `json:"respSize"`
	RespTrunc   bool        `json:"respTrunc"`
	RespSkipped bool        `json:"respSkipped"`
	Err         string      `json:"err,omitempty"`
}

// Store — кольцевой буфер записей с ленивым сохранением на диск.
type Store struct {
	mu       sync.RWMutex
	entries  []*Entry // от старых к новым
	nextID   int64
	path     string
	lines    int           // строк в файле; трогает только RunPersister
	pending  chan *Entry   // новые записи на дозапись
	clearSig chan struct{} // просьба обнулить файл
	done     chan struct{} // закрывается, когда персистер дописал всё
	// needRewrite: файл при загрузке оказался грязным (битые или
	// задвоенные строки) — перепишем его один раз на старте.
	needRewrite bool
}

func NewStore(path string) *Store {
	s := &Store{
		path:     path,
		nextID:   1,
		pending:  make(chan *Entry, 256),
		clearSig: make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	s.load()
	return s
}

// История пишется построчно (JSON Lines): одна строка на запись.
// Полное перемаршаливание всей истории каждые пару секунд раздувало RSS
// до 40 МБ на забитом буфере — вся экономия на телах уходила в мусор
// от json.Marshal. Теперь на диск уходит ровно одна новая запись,
// а файл целиком переписывается только при компактификации.
const compactAfter = 2 * maxEntries

func (s *Store) load() {
	f, err := os.Open(s.path)
	if err != nil {
		return
	}

	// Читаем построчно и пропускаем то, что не разобралось: аварийное
	// завершение могло оставить недописанную строку, и раньше всё, что
	// лежало после неё, терялось при следующем старте.
	seen := make(map[int64]*Entry)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	bad := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(line, &e) != nil {
			bad++
			continue
		}
		ec := e
		// Дедупликация по id: компактификация может записать запись,
		// которая ещё висела в очереди, второй раз.
		if _, dup := seen[e.ID]; dup {
			bad++
		}
		seen[e.ID] = &ec
	}
	_ = f.Close()

	if len(seen) == 0 {
		s.needRewrite = bad > 0
		return
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids) // порядок в файле мог разойтись с порядком id
	for _, id := range ids {
		s.entries = append(s.entries, seen[id])
		if id >= s.nextID {
			s.nextID = id + 1
		}
	}
	s.lines = len(s.entries)
	if len(s.entries) > maxEntries {
		s.entries = s.entries[len(s.entries)-maxEntries:]
		s.lines = maxEntries
	}
	// Файл был грязный либо длиннее буфера — перепишем его один раз на старте.
	s.needRewrite = bad > 0 || len(seen) != len(s.entries)
}

func (s *Store) Add(e *Entry) {
	s.mu.Lock()
	e.ID = s.nextID
	s.nextID++
	s.entries = append(s.entries, e)
	if len(s.entries) > maxEntries {
		// освобождаем ссылки на вытесненные записи, чтобы их забрал GC
		drop := len(s.entries) - maxEntries
		copy(s.entries, s.entries[drop:])
		for i := len(s.entries) - drop; i < len(s.entries); i++ {
			s.entries[i] = nil
		}
		s.entries = s.entries[:maxEntries]
	}
	s.mu.Unlock()

	// Диск не должен тормозить проксирование: не влезло в очередь —
	// запись просто не попадёт в файл, в памяти она всё равно есть.
	select {
	case s.pending <- e:
	default:
	}
}

// List возвращает лёгкие записи (без тел) новее указанного id, новые первыми.
func (s *Store) List(since int64) []*Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Entry, 0, len(s.entries))
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
		if e.ID <= since {
			break
		}
		light := *e
		light.ReqBody, light.RespBody = nil, nil
		light.ReqHeaders, light.RespHeaders = nil, nil
		out = append(out, &light)
	}
	return out
}

func (s *Store) Get(id int64) *Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}

func (s *Store) Clear() {
	s.mu.Lock()
	for i := range s.entries {
		s.entries[i] = nil
	}
	s.entries = s.entries[:0]
	s.mu.Unlock()
	select {
	case s.clearSig <- struct{}{}:
	default:
	}
}

// RunPersister — единственный, кто пишет в файл истории. Он же единственный
// владелец дескриптора: compact закрывает старый файл и возвращает новый,
// иначе на каждой компактификации утекал бы один fd, и процесс рано или
// поздно упёрся бы в лимит, не упав — то есть launchd его бы не перезапустил.
func (s *Store) RunPersister(stop <-chan struct{}) {
	defer close(s.done)

	f := s.openAppend()
	var w *bufio.Writer
	if f != nil {
		w = bufio.NewWriterSize(f, 64*1024)
		if s.needRewrite {
			f, w = s.compact(f, w)
		}
	}
	defer func() {
		if w != nil {
			_ = w.Flush()
		}
		if f != nil {
			_ = f.Close()
		}
	}()

	write := func(e *Entry) {
		if w == nil {
			return
		}
		b, err := json.Marshal(e)
		if err != nil {
			return
		}
		_, _ = w.Write(b)
		_ = w.WriteByte('\n')
		s.lines++
		if s.lines > compactAfter {
			f, w = s.compact(f, w)
		}
	}

	flush := time.NewTicker(2 * time.Second)
	defer flush.Stop()

	for {
		select {
		case e := <-s.pending:
			write(e)
		case <-s.clearSig:
			f, w = s.recreate(f, w)
		case <-flush.C:
			if w != nil {
				_ = w.Flush()
			}
		case <-stop:
			// Добираем хвост очереди, иначе последние запросы перед
			// остановкой не попали бы на диск.
			for {
				select {
				case e := <-s.pending:
					write(e)
					continue
				default:
				}
				break
			}
			return
		}
	}
}

// openAppend открывает файл истории на дозапись с правами 0600:
// там лежат куки и заголовки авторизации из живого трафика.
func (s *Store) openAppend() *os.File {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("история недоступна для записи (%v) — работаем только в памяти", err)
		return nil
	}
	_ = os.Chmod(s.path, 0o600)
	return f
}

// compact переписывает файл текущим содержимым буфера, выбрасывая
// вытесненные записи, и передаёт владение новым дескриптором.
func (s *Store) compact(f *os.File, w *bufio.Writer) (*os.File, *bufio.Writer) {
	// Всё, что ещё висит в очереди, уже лежит в кольце и попадёт в снимок.
	// Если это не выбрать сейчас, те же записи уйдут в файл второй раз.
drain:
	for {
		select {
		case <-s.pending:
		default:
			break drain
		}
	}

	s.mu.RLock()
	snapshot := make([]*Entry, len(s.entries))
	copy(snapshot, s.entries)
	s.mu.RUnlock()

	tmp := s.path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return f, w
	}
	tw := bufio.NewWriterSize(tf, 64*1024)
	enc := json.NewEncoder(tw) // Encode сам ставит \n — ровно JSON Lines
	ok := true
	for _, e := range snapshot {
		if enc.Encode(e) != nil {
			ok = false
			break
		}
	}
	if !ok || tw.Flush() != nil || tf.Close() != nil {
		os.Remove(tmp)
		return f, w
	}
	if os.Rename(tmp, s.path) != nil {
		os.Remove(tmp)
		return f, w
	}

	// Старый дескриптор больше не нужен — закрываем, иначе утечка.
	if w != nil {
		_ = w.Flush()
	}
	if f != nil {
		_ = f.Close()
	}
	s.lines = len(snapshot)
	nf := s.openAppend()
	if nf == nil {
		return nil, nil
	}
	if w == nil {
		return nf, bufio.NewWriterSize(nf, 64*1024)
	}
	w.Reset(nf)
	return nf, w
}

// recreate обнуляет файл после очистки истории из UI.
func (s *Store) recreate(f *os.File, w *bufio.Writer) (*os.File, *bufio.Writer) {
	if w != nil {
		w.Reset(io.Discard) // сбрасываем недописанное, файл всё равно уйдёт
	}
	if f != nil {
		_ = f.Close()
	}
	_ = os.Remove(s.path)
	s.lines = 0
	nf := s.openAppend()
	if nf == nil {
		return nil, nil
	}
	if w == nil {
		return nf, bufio.NewWriterSize(nf, 64*1024)
	}
	w.Reset(nf)
	return nf, w
}

// Done закрывается, когда персистер дописал всё и закрыл файл.
func (s *Store) Done() <-chan struct{} { return s.done }

func ensureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

// capturable решает, стоит ли вообще копировать тело в память.
// Бинарь (картинки, видео, архивы) не копируем — только размер.
func capturable(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if ct == "" {
		return true // неизвестный тип: скорее всего текст или пусто
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	for _, s := range []string{"json", "xml", "javascript", "ecmascript",
		"x-www-form-urlencoded", "graphql", "yaml", "x-ndjson", "csv", "html"} {
		if strings.Contains(ct, s) {
			return true
		}
	}
	return false
}

// capReader читает поток, попутно складывая в буфер не более cap байт.
type capReader struct {
	src   io.ReadCloser
	buf   bytes.Buffer
	skip  bool
	total int64
	trunc bool
	done  func(*capReader)
	once  sync.Once
}

func newCapReader(src io.ReadCloser, skip bool, done func(*capReader)) *capReader {
	return &capReader{src: src, skip: skip, done: done}
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.src.Read(p)
	if n > 0 {
		c.total += int64(n)
		if !c.skip {
			if rem := bodyCap - c.buf.Len(); rem >= n {
				c.buf.Write(p[:n])
			} else {
				if rem > 0 {
					c.buf.Write(p[:rem])
				}
				c.trunc = true
			}
		}
	}
	if err != nil {
		c.finish()
	}
	return n, err
}

func (c *capReader) Close() error {
	c.finish()
	return c.src.Close()
}

func (c *capReader) finish() {
	c.once.Do(func() {
		if c.done != nil {
			c.done(c)
		}
	})
}

// joinedBody склеивает уже прочитанный хвост с остатком исходного потока.
type joinedBody struct {
	io.Reader
	closer io.Closer
}

func (j *joinedBody) Close() error { return j.closer.Close() }

// prereadBody читает начало тела немедленно и возвращает тело, пригодное
// для дальнейшей передачи апстриму. В память попадает не более bodyCap байт.
// size == -1 означает "точный размер неизвестен" (тело больше лимита либо
// бинарное и шло без Content-Length).
func prereadBody(body io.ReadCloser, skip bool, contentLength int64) (
	head []byte, out io.ReadCloser, size int64, trunc bool) {

	if skip {
		// Бинарь не копируем вообще — только пропускаем насквозь.
		return nil, body, contentLength, false
	}

	buf := make([]byte, bodyCap+1)
	n, _ := io.ReadFull(body, buf)

	if n <= bodyCap {
		// Тело целиком у нас: отдаём апстриму буфер, размер точный.
		_ = body.Close()
		return buf[:n], io.NopCloser(bytes.NewReader(buf[:n])), int64(n), false
	}

	// Тело больше лимита: начало сохраняем, остальное стримим насквозь,
	// ничего не считая — иначе пришлось бы писать в запись после store.Add.
	size = contentLength
	if size < 0 {
		size = -1
	}
	return buf[:bodyCap],
		&joinedBody{Reader: io.MultiReader(bytes.NewReader(buf[bodyCap:n]), body),
			closer: body},
		size, true
}
