package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const logRetention = 7 * 24 * time.Hour
const maxLogRecords = 10000
const maxLogDiskBytes int64 = 256 << 20

var logIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type storedLog struct {
	summary requestSummary
	size    int64
	entry   *requestLog
}
type requestLogStore struct {
	mu        sync.Mutex
	dir       string
	entries   map[string]storedLog
	bytes     int64
	lastError string
}
type logStorageInfo struct {
	RetentionDays int    `json:"retention_days"`
	MaxRecords    int    `json:"max_records"`
	MaxBytes      int64  `json:"max_bytes"`
	Records       int    `json:"records"`
	Bytes         int64  `json:"bytes"`
	LastError     string `json:"last_error"`
}

func newRequestLogStore(dir string) (*requestLogStore, error) {
	store := &requestLogStore{dir: dir, entries: map[string]storedLog{}}
	if dir == "" {
		return store, nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("cannot create request log directory")
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read request log directory")
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(file.Name(), ".json")
		if !logIDPattern.MatchString(id) {
			continue
		}
		path := filepath.Join(dir, file.Name())
		stat, err := file.Info()
		if err != nil {
			return nil, fmt.Errorf("cannot inspect request log")
		}
		if stat.Size() > maxLogDiskBytes || stat.ModTime().Before(time.Now().Add(-logRetention)) {
			if os.Remove(path) != nil {
				return nil, fmt.Errorf("cannot rotate expired request logs")
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read request log")
		}
		var entry requestLog
		if json.Unmarshal(data, &entry) != nil || entry.ID != id || entry.Started.IsZero() || entry.Finished.IsZero() {
			store.lastError = "存在损坏的日志文件，已跳过"
			continue
		}
		if err := os.Chmod(path, 0600); err != nil {
			return nil, fmt.Errorf("cannot secure request log permissions")
		}
		store.entries[id] = storedLog{summary: entry.requestSummary, size: stat.Size()}
		store.bytes += stat.Size()
	}
	if err := store.prune(time.Now()); err != nil {
		return nil, err
	}
	return store, nil
}
func (s *requestLogStore) info() logStorageInfo {
	return logStorageInfo{7, maxLogRecords, maxLogDiskBytes, len(s.entries), s.bytes, s.lastError}
}
func (s *requestLogStore) prune(now time.Time) error {
	records := make([]storedLog, 0, len(s.entries))
	for _, entry := range s.entries {
		records = append(records, entry)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].summary.Started.Before(records[j].summary.Started) })
	for _, entry := range records {
		if entry.summary.Finished.After(now.Add(-logRetention)) && len(s.entries) <= maxLogRecords && s.bytes <= maxLogDiskBytes {
			continue
		}
		if s.dir != "" {
			if err := os.Remove(filepath.Join(s.dir, entry.summary.ID+".json")); err != nil && !os.IsNotExist(err) {
				s.lastError = "旧日志清理失败，请检查 data/logs 写入权限"
				return fmt.Errorf("cannot rotate request logs")
			}
		}
		delete(s.entries, entry.summary.ID)
		s.bytes -= entry.size
	}
	return nil
}
func (s *requestLogStore) add(entry requestLog) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("cannot encode request log")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if int64(len(data)) > maxLogDiskBytes {
		s.lastError = "单条日志超过存储上限，未保存"
		return fmt.Errorf("request log too large")
	}
	if s.dir != "" {
		err = s.write(entry.ID, data)
		if err != nil {
			s.lastError = "日志写入失败，请检查 data/logs 空间与权限"
			return err
		}
	}
	if old, ok := s.entries[entry.ID]; ok {
		s.bytes -= old.size
	}
	stored := storedLog{summary: entry.requestSummary, size: int64(len(data))}
	if s.dir == "" {
		stored.entry = &entry
	}
	s.entries[entry.ID] = stored
	s.bytes += stored.size
	s.lastError = ""
	return s.prune(time.Now())
}
func (s *requestLogStore) write(id string, data []byte) error {
	file, err := os.CreateTemp(s.dir, ".request-*")
	if err != nil {
		return fmt.Errorf("cannot create request log")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err != nil {
		return fmt.Errorf("cannot secure request log")
	}
	if _, err = file.Write(data); err != nil {
		return fmt.Errorf("cannot write request log")
	}
	if err = file.Sync(); err != nil {
		return fmt.Errorf("cannot sync request log")
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("cannot close request log")
	}
	if err = os.Rename(file.Name(), filepath.Join(s.dir, id+".json")); err != nil {
		return fmt.Errorf("cannot commit request log")
	}
	return nil
}
func (s *requestLogStore) get(id string) (requestLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !logIDPattern.MatchString(id) {
		return requestLog{}, &apiFailure{404, "日志不存在或已过期"}
	}
	entry, ok := s.entries[id]
	if !ok {
		return requestLog{}, &apiFailure{404, "日志不存在或已过期"}
	}
	if entry.entry != nil {
		return *entry.entry, nil
	}
	file, err := os.Open(filepath.Join(s.dir, id+".json"))
	if err != nil {
		return requestLog{}, &apiFailure{500, "无法读取日志文件"}
	}
	defer file.Close()
	var result requestLog
	if json.NewDecoder(io.LimitReader(file, maxLogDiskBytes+1)).Decode(&result) != nil {
		return requestLog{}, &apiFailure{500, "日志文件已损坏"}
	}
	return result, nil
}

type logFilter struct {
	rangeName, model, account, status, query string
	since                                    time.Time
	page, size                               int
}

func parseLogFilter(r *http.Request) (logFilter, error) {
	q := r.URL.Query()
	f := logFilter{rangeName: q.Get("range"), model: q.Get("model"), account: q.Get("account"), status: q.Get("status"), query: strings.ToLower(strings.TrimSpace(q.Get("q"))), page: 1, size: 25}
	if f.rangeName == "" {
		f.rangeName = "24h"
	}
	switch f.rangeName {
	case "24h":
		f.since = time.Now().Add(-24 * time.Hour)
	case "7d", "all":
		f.since = time.Now().Add(-logRetention)
	default:
		return f, invalid("range 必须为 24h、7d 或 all")
	}
	switch f.status {
	case "", "success", "error", "canceled", "interrupted":
	default:
		return f, invalid("无效的日志状态")
	}
	if len(f.query) > 256 || len(f.model) > 256 || len(f.account) > 128 {
		return f, invalid("筛选条件过长")
	}
	for _, key := range []string{"page", "page_size"} {
		if value := q.Get(key); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || key == "page" && n > 1000000 || key == "page_size" && n > 100 {
				return f, invalid("无效的分页参数")
			}
			if key == "page" {
				f.page = n
			} else {
				f.size = n
			}
		}
	}
	return f, nil
}
func (s *requestLogStore) matching(f logFilter) ([]requestSummary, logStorageInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(time.Now())
	result := []requestSummary{}
	for _, stored := range s.entries {
		v := stored.summary
		if v.Started.Before(f.since) || f.model != "" && v.Model != f.model || f.account != "" && v.AccountID != f.account || f.status != "" && v.Outcome != f.status {
			continue
		}
		if f.query != "" && !strings.Contains(strings.ToLower(strings.Join([]string{v.ID, v.Model, v.UpstreamModel, v.AccountName, v.Path, v.Error, strconv.Itoa(v.Status)}, " ")), f.query) {
			continue
		}
		result = append(result, v)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Started.Equal(result[j].Started) {
			return result[i].ID > result[j].ID
		}
		return result[i].Started.After(result[j].Started)
	})
	return result, s.info()
}
