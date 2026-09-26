package adminweb

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"mlstoredb/db"
)

type rowError struct {
	Row   int    `json:"row"`
	Error string `json:"error"`
}

func detectFormat(data, chosen string) string {
	if chosen != "" && chosen != "auto" {
		return chosen
	}
	s := strings.TrimSpace(data)
	if strings.HasPrefix(s, "[") {
		return "json"
	}
	if strings.HasPrefix(s, "{") {
		lines := strings.Split(s, "\n")
		count := 0
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				count++
				if count > 1 {
					return "ndjson"
				}
			}
		}
		return "json"
	}
	return "csv"
}

func (s *Server) handleListIndexes(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	idxs, err := ws.sess.ListIndexes(coll)
	if err != nil {
		writeError(w, err)
		return
	}
	type ixInfo struct {
		Name   string   `json:"name"`
		Fields []string `json:"fields"`
		Unique bool     `json:"unique"`
	}
	list := make([]ixInfo, 0, len(idxs))
	for _, ix := range idxs {
		list = append(list, ixInfo{Name: strings.Join(ix.Fields, "_") + "_1", Fields: ix.Fields, Unique: ix.Unique})
	}
	writeJSON(w, http.StatusOK, map[string]any{"indexes": list})
}

func (s *Server) handleCreateIndex(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	var body struct {
		Fields []string `json:"fields"`
		Unique bool     `json:"unique"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.Fields) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fields required"})
		return
	}
	for _, f := range body.Fields {
		if f == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty field"})
			return
		}
	}
	if err := ws.sess.EnsureIndex(coll, body.Fields, body.Unique); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *Server) handleDropIndex(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	fieldsParam := r.URL.Query().Get("fields")
	if fieldsParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fields required"})
		return
	}
	fields := strings.Split(fieldsParam, ",")
	if err := ws.sess.DropIndex(coll, fields); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func csvColumns(docs []db.Document) []string {
	seen := map[string]bool{}
	var cols []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			cols = append(cols, k)
		}
	}
	add("_id")
	names := make([]string, 0, 8)
	for _, d := range docs {
		names = names[:0]
		for k := range d {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			add(k)
		}
	}
	return cols
}

func csvCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func writeCSV(w http.ResponseWriter, docs []db.Document) {
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	if len(docs) == 0 {
		return
	}
	cols := csvColumns(docs)
	cw := csv.NewWriter(w)
	_ = cw.Write(cols)
	for _, d := range docs {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = csvCell(d[c])
		}
		_ = cw.Write(row)
	}
	cw.Flush()
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	q := r.URL.Query()
	format := q.Get("format")
	if format == "" {
		format = "json"
	}
	filter, err := parseFilter(q.Get("filter"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	docs, err := ws.sess.Find(coll, filter, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	switch format {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", coll+".csv"))
		writeCSV(w, docs)
	case "ndjson":
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", coll+".ndjson"))
		enc := json.NewEncoder(w)
		for _, d := range docs {
			_ = enc.Encode(d)
		}
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", coll+".json"))
		_, _ = w.Write([]byte("["))
		for i, d := range docs {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			b, _ := json.Marshal(d)
			_, _ = w.Write(b)
		}
		_, _ = w.Write([]byte("]"))
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "format must be csv, json or ndjson"})
	}
}

type importBody struct {
	Format  string `json:"format"`
	Data    string `json:"data"`
	Mode    string `json:"mode"`
	Preview bool   `json:"preview"`
}

func parseCSVData(data string) ([]db.Document, error) {
	reader := csv.NewReader(strings.NewReader(data))
	reader.TrimLeadingSpace = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, errors.New("invalid CSV: " + err.Error())
	}
	if len(records) == 0 {
		return nil, errors.New("empty CSV")
	}
	header := records[0]
	var docs []db.Document
	for _, rec := range records[1:] {
		if len(rec) == 1 && rec[0] == "" {
			continue
		}
		doc := db.Document{}
		for i, cell := range rec {
			if i >= len(header) {
				break
			}
			key := strings.TrimSpace(header[i])
			if key == "" {
				continue
			}
			if key == "_id" {
				idv := strings.TrimSpace(cell)
				if idv == "" {
					continue
				}
				doc[key] = idv
				continue
			}
			doc[key] = inferCSVValue(cell)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs, nil
}

func inferCSVValue(cell string) any {
	cell = strings.TrimSpace(cell)
	switch cell {
	case "":
		return nil
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if n, err := strconv.ParseFloat(cell, 64); err == nil {
		return n
	}
	return cell
}

func parseJSONImport(data string) ([]db.Document, error) {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return nil, errors.New("empty data")
	}
	if strings.HasPrefix(trimmed, "[") {
		var docs []db.Document
		if err := json.Unmarshal([]byte(trimmed), &docs); err != nil {
			return nil, errors.New("invalid JSON array: " + err.Error())
		}
		return docs, nil
	}
	var doc db.Document
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return nil, errors.New("invalid JSON object: " + err.Error())
	}
	return []db.Document{doc}, nil
}

func parseNDJSON(data string) ([]db.Document, error) {
	var docs []db.Document
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var doc db.Document
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			return nil, fmt.Errorf("line %d: %v", i+1, err)
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		return nil, errors.New("empty data")
	}
	return docs, nil
}

func parseImportDocs(format, data string) ([]db.Document, error) {
	switch format {
	case "csv":
		return parseCSVData(data)
	case "json":
		return parseJSONImport(data)
	case "ndjson":
		return parseNDJSON(data)
	default:
		return nil, errors.New("format must be csv, json or ndjson")
	}
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request, ws *webSession) {
	coll := r.PathValue("name")
	if coll == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "collection required"})
		return
	}
	format := r.URL.Query().Get("format")
	mode := r.URL.Query().Get("mode")
	preview := r.URL.Query().Get("preview") == "1" || r.URL.Query().Get("preview") == "true"

	if preview {
		docs, total, err := parseImportPreview(format, r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"total": total, "preview": docs})
		return
	}

	job := s.importJobCreate(format, mode)
	kw := &importWorker{s: s, ws: ws, coll: coll, job: job, stream: r.Body}
	go kw.run()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"jobId":  string(job.ID),
		"status": "running",
	})
}

const importPreviewBytes = 64 * 1024

func parseImportPreview(format string, rc io.ReadCloser) ([]db.Document, int, error) {
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, importPreviewBytes))
	if err != nil {
		return nil, 0, err
	}
	switch format {
	case "csv":
		docs, err := parseCSVData(string(data))
		if err != nil {
			return nil, 0, err
		}
		preview := docs
		if len(preview) > 10 {
			preview = preview[:10]
		}
		return preview, len(docs), nil
	case "ndjson":
		docs, err := parseNDJSON(string(data))
		if err != nil {
			return nil, 0, err
		}
		preview := docs
		if len(preview) > 10 {
			preview = preview[:10]
		}
		return preview, len(docs), nil
	default:
		var docs []db.Document
		if err := json.Unmarshal(data, &docs); err != nil {
			return nil, 0, errors.New("invalid JSON array: " + err.Error())
		}
		preview := docs
		if len(preview) > 10 {
			preview = preview[:10]
		}
		return preview, len(docs), nil
	}
}

const asyncImportBatch = 100
const importMaxBytes = 4 << 30

type importWorker struct {
	s      *Server
	ws     *webSession
	coll   string
	job    *importJob
	stream io.ReadCloser
}

func (w *importWorker) run() {
	defer func() {
		if rec := recover(); rec != nil {
			w.job.mu.Lock()
			w.job.Status = "error"
			w.job.ErrMsg = fmt.Sprintf("panic: %v", rec)
			w.job.mu.Unlock()
		}
		if w.stream != nil {
			w.stream.Close()
		}
	}()
	format := w.job.Format
	if format == "csv" || format == "ndjson" || format == "auto" {
		format = detectFormatReader(w.stream, format)
	}

	workers := asyncImportWorkers()
	limited := io.LimitReader(w.stream, importMaxBytes)
	workCh := make(chan []db.Document, workers*2)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go w.importPump(&wg, workCh)
	}

	w.streamDocsToChannel(format, limited, workCh)
	close(workCh)
	wg.Wait()

	w.job.mu.Lock()
	w.job.Status = "done"
	w.job.mu.Unlock()
}

func asyncImportWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	return n
}

func (w *importWorker) importPump(wg *sync.WaitGroup, workCh <-chan []db.Document) {
	defer wg.Done()
	for batch := range workCh {
		for _, doc := range batch {
			opErr := insertOne(w.coll, doc, w.job.Mode, w.ws)
			w.job.mu.Lock()
			if opErr != nil {
				w.job.Errors++
			} else {
				w.job.Inserted++
			}
			w.job.Processed++
			w.job.mu.Unlock()
		}
	}
}

func (w *importWorker) streamDocsToChannel(format string, r io.Reader, workCh chan<- []db.Document) {
	var docCh <-chan db.Document
	switch format {
	case "csv":
		docCh = parseCSVStream(r)
	case "ndjson":
		docCh = parseNDJSONStreamReader(r)
	default:
		docCh = parseJSONArrayStreamReader(r)
	}
	batch := make([]db.Document, 0, asyncImportBatch)
	for doc := range docCh {
		batch = append(batch, doc)
		w.job.mu.Lock()
		w.job.Total++
		w.job.mu.Unlock()
		if len(batch) >= asyncImportBatch {
			workCh <- batch
			batch = make([]db.Document, 0, asyncImportBatch)
		}
	}
	if len(batch) > 0 {
		workCh <- batch
	}
}

func detectFormatReader(r io.Reader, chosen string) string {
	if chosen != "" && chosen != "auto" {
		return chosen
	}
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(r, buf)
	if n == 0 {
		return "json"
	}
	s := strings.TrimSpace(string(buf[:n]))
	if strings.HasPrefix(s, "[") {
		return "json"
	}
	if strings.HasPrefix(s, "{") {
		lines := strings.Split(s, "\n")
		cnt := 0
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				cnt++
				if cnt > 1 {
					return "ndjson"
				}
			}
		}
		return "json"
	}
	return "csv"
}

func parseCSVStream(r io.Reader) <-chan db.Document {
	docCh := make(chan db.Document, 256)
	go func() {
		defer close(docCh)
		reader := csv.NewReader(r)
		reader.TrimLeadingSpace = true
		records, err := reader.ReadAll()
		if err != nil {
			return
		}
		if len(records) == 0 {
			return
		}
		header := records[0]
		for _, rec := range records[1:] {
			if len(rec) == 1 && rec[0] == "" {
				continue
			}
			doc := db.Document{}
			for i, cell := range rec {
				if i >= len(header) {
					break
				}
				key := strings.TrimSpace(header[i])
				if key == "" {
					continue
				}
				if key == "_id" {
					idv := strings.TrimSpace(cell)
					if idv == "" {
						continue
					}
					doc[key] = idv
					continue
				}
				doc[key] = inferCSVValue(cell)
			}
			if len(doc) > 0 {
				docCh <- doc
			}
		}
	}()
	return docCh
}

func parseNDJSONStreamReader(r io.Reader) <-chan db.Document {
	out := make(chan db.Document, 256)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(r)
		buf := make([]byte, 128*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var doc db.Document
			if err := json.Unmarshal([]byte(line), &doc); err != nil {
				continue
			}
			out <- doc
		}
	}()
	return out
}

func parseJSONArrayStreamReader(r io.Reader) <-chan db.Document {
	out := make(chan db.Document, 256)
	go func() {
		defer close(out)
		dec := json.NewDecoder(r)
		tok, err := dec.Token()
		if err != nil {
			return
		}
		if delim, ok := tok.(json.Delim); !ok || delim != '[' {
			return
		}
		for dec.More() {
			var doc db.Document
			if err := dec.Decode(&doc); err != nil {
				continue
			}
			out <- doc
		}
	}()
	return out
}

func insertOne(coll string, doc db.Document, mode string, ws *webSession) error {
	if mode == "upsert" {
		id, _ := doc["_id"].(string)
		if id == "" {
			return ws.sess.Insert(coll, doc)
		}
		return ws.sess.Upsert(coll, id, doc)
	}
	return ws.sess.Insert(coll, doc)
}

func (s *Server) handleImportStatus(w http.ResponseWriter, r *http.Request, ws *webSession) {
	id := importJobID(r.PathValue("id"))
	job := s.importJobGet(id)
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "job not found"})
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

func (s *Server) handleListTriggers(w http.ResponseWriter, r *http.Request, ws *webSession) {
	triggers, err := s.store.ListTriggers()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggers": triggers})
}

func (s *Server) handleUpsertTrigger(w http.ResponseWriter, r *http.Request, ws *webSession) {
	var t db.Trigger
	if !readJSON(w, r, &t) {
		return
	}
	if t.Collection == "" || t.Collection == "*" {
		if !ws.sess.CanWrite("*") {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
			return
		}
	} else if !ws.sess.CanWrite(t.Collection) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}
	if t.ID != "" {
		_ = s.store.DeleteTrigger(t.ID)
	}
	id, err := s.store.CreateTrigger(t)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

func (s *Server) handleDeleteTrigger(w http.ResponseWriter, r *http.Request, ws *webSession) {
	id := r.PathValue("id")
	triggers, err := s.store.ListTriggers()
	if err != nil {
		writeError(w, err)
		return
	}
	for _, t := range triggers {
		if t.ID == id {
			if t.Collection == "" || t.Collection == "*" {
				if !ws.sess.CanWrite("*") {
					writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
					return
				}
			} else if !ws.sess.CanWrite(t.Collection) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
				return
			}
			break
		}
	}
	if err := s.store.DeleteTrigger(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
