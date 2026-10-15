// Package store 是全部业务数据的 SQLite 存储：设置、资料/素材/话术、检索片段（含向量）、客户与聊天记录。
//
// 使用纯 Go 的 modernc.org/sqlite，无需 CGO，便于交叉编译出单文件程序。
package store

import (
	"database/sql"
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS documents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  kind TEXT NOT NULL,            -- doc 资料 / history 历史聊天 / example 客服改写的示范回复
  path TEXT NOT NULL DEFAULT '',
  hash TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,          -- processing / done / error
  error TEXT NOT NULL DEFAULT '',
  chunks INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS chunks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  doc_id INTEGER NOT NULL,
  kind TEXT NOT NULL,
  content TEXT NOT NULL,
  embedding BLOB
);
CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(doc_id);

CREATE TABLE IF NOT EXISTS customers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  platform TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  tags TEXT NOT NULL DEFAULT '',
  profile TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  customer_id INTEGER NOT NULL,
  role TEXT NOT NULL,            -- customer / agent
  content TEXT NOT NULL,
  translation TEXT NOT NULL DEFAULT '',
  image_path TEXT NOT NULL DEFAULT '',
  analysis TEXT NOT NULL DEFAULT '',   -- 客户消息：AI 分析 JSON
  ai_reply TEXT NOT NULL DEFAULT '',   -- 客服消息：AI 原始建议
  action TEXT NOT NULL DEFAULT '',     -- accepted / edited / manual
  reply_to INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_customer ON messages(customer_id);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	// 增量迁移：已存在的列会报 duplicate column，忽略即可
	for _, m := range []string{
		`ALTER TABLE documents ADD COLUMN media_type TEXT NOT NULL DEFAULT ''`, // image / video（仅 kind=media）
		`ALTER TABLE documents ADD COLUMN description TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE documents ADD COLUMN tags TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE documents ADD COLUMN usage TEXT NOT NULL DEFAULT ''`, // 什么时候适合发
		`ALTER TABLE documents ADD COLUMN thumb TEXT NOT NULL DEFAULT ''`, // 视频关键帧拼图
		`ALTER TABLE documents ADD COLUMN send_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE messages ADD COLUMN media_ids TEXT NOT NULL DEFAULT ''`, // 随回复发送的素材
	} {
		if _, err := db.Exec(m); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	return &Store{db: db}, nil
}

func now() int64 { return time.Now().Unix() }

// Backup 把数据库完整快照写到 path（SQLite VACUUM INTO，运行中也能安全备份）
func (s *Store) Backup(path string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

// HasData 是否已有资料或客户（用于判断升级前要不要自动备份）
func (s *Store) HasData() bool {
	var n int
	_ = s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM documents) + (SELECT COUNT(*) FROM customers)`).Scan(&n)
	return n > 0
}

// ---------- settings ----------

func (s *Store) Settings() map[string]string {
	m := map[string]string{}
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		_ = rows.Scan(&k, &v)
		m[k] = v
	}
	return m
}

func (s *Store) SaveSettings(m map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for k, v := range m {
		if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ---------- documents & chunks ----------

type Document struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Path      string `json:"-"`
	Hash      string `json:"-"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	Chunks    int    `json:"chunks"`
	CreatedAt int64  `json:"created_at"`
	// 以下仅素材库（kind=media）使用
	MediaType   string `json:"media_type,omitempty"`
	Description string `json:"description,omitempty"`
	Tags        string `json:"tags,omitempty"`
	Usage       string `json:"usage,omitempty"`
	Thumb       string `json:"-"`
	SendCount   int    `json:"send_count,omitempty"`
	URL         string `json:"url,omitempty"`       // 原文件访问地址
	ThumbURL    string `json:"thumb_url,omitempty"` // 缩略图地址
}

const docCols = `id,name,kind,path,hash,status,error,chunks,created_at,media_type,description,tags,usage,thumb,send_count`

func scanDoc(sc interface{ Scan(...any) error }) (Document, error) {
	var d Document
	err := sc.Scan(&d.ID, &d.Name, &d.Kind, &d.Path, &d.Hash, &d.Status, &d.Error, &d.Chunks, &d.CreatedAt,
		&d.MediaType, &d.Description, &d.Tags, &d.Usage, &d.Thumb, &d.SendCount)
	return d, err
}

func (s *Store) AddDocument(d *Document) error {
	res, err := s.db.Exec(`INSERT INTO documents(name,kind,path,hash,status,created_at,media_type,description,tags,usage) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		d.Name, d.Kind, d.Path, d.Hash, d.Status, now(), d.MediaType, d.Description, d.Tags, d.Usage)
	if err != nil {
		return err
	}
	d.ID, _ = res.LastInsertId()
	return nil
}

// UpdateMediaInfo 保存素材的描述/标签/适用场景/缩略图
func (s *Store) UpdateMediaInfo(d *Document) error {
	_, err := s.db.Exec(`UPDATE documents SET name=?,description=?,tags=?,usage=?,thumb=? WHERE id=?`,
		d.Name, d.Description, d.Tags, d.Usage, d.Thumb, d.ID)
	return err
}

// ReplaceChunks 删除文档旧片段并写入新片段（素材信息修改后重建索引）
func (s *Store) ReplaceChunks(docID int64, cs []Chunk) ([]int64, error) {
	if _, err := s.db.Exec(`DELETE FROM chunks WHERE doc_id=?`, docID); err != nil {
		return nil, err
	}
	return s.AddChunks(cs)
}

func (s *Store) IncSendCount(ids []int64) {
	for _, id := range ids {
		_, _ = s.db.Exec(`UPDATE documents SET send_count=send_count+1 WHERE id=?`, id)
	}
}

// SentMedia 某客户已经收到过的素材 ID
func (s *Store) SentMedia(customerID int64) map[int64]bool {
	out := map[int64]bool{}
	rows, err := s.db.Query(`SELECT media_ids FROM messages WHERE customer_id=? AND media_ids<>''`, customerID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		for _, id := range ParseIDs(v) {
			out[id] = true
		}
	}
	return out
}

func ParseIDs(s string) []int64 {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

func JoinIDs(ids []int64) string {
	ss := make([]string, len(ids))
	for i, id := range ids {
		ss[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(ss, ",")
}

func (s *Store) DocByHash(hash, kind string) (int64, bool) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM documents WHERE hash=? AND hash<>'' AND kind=? AND status<>'error'`, hash, kind).Scan(&id)
	return id, err == nil
}

func (s *Store) UpdateDocStatus(id int64, status, errMsg string, chunks int) {
	_, _ = s.db.Exec(`UPDATE documents SET status=?, error=?, chunks=? WHERE id=?`, status, errMsg, chunks, id)
}

// ListDocuments media=true 只返回素材库，false 返回文件资料（不含素材和自定义话术）
func (s *Store) ListDocuments(media bool) ([]Document, error) {
	where := `kind NOT IN ('media','snippet')`
	if media {
		where = `kind='media'`
	}
	return s.listDocs(where)
}

// ListSnippets 自定义话术，最近修改的在前
func (s *Store) ListSnippets() ([]Document, error) { return s.listDocs(`kind='snippet'`) }

func (s *Store) listDocs(where string) ([]Document, error) {
	rows, err := s.db.Query(`SELECT ` + docCols + ` FROM documents WHERE ` + where + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Document{}
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *Store) GetDocument(id int64) (*Document, error) {
	d, err := scanDoc(s.db.QueryRow(`SELECT `+docCols+` FROM documents WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Store) DeleteDocument(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM chunks WHERE doc_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM documents WHERE id=?`, id)
	return err
}

type Chunk struct {
	ID        int64
	DocID     int64
	Kind      string
	Content   string
	Embedding []float32
}

func (s *Store) AddChunks(cs []Chunk) ([]int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(cs))
	for _, c := range cs {
		res, err := tx.Exec(`INSERT INTO chunks(doc_id,kind,content,embedding) VALUES(?,?,?,?)`,
			c.DocID, c.Kind, c.Content, encodeVec(c.Embedding))
		if err != nil {
			tx.Rollback()
			return nil, err
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	return ids, tx.Commit()
}

func (s *Store) AllChunks() ([]Chunk, error) {
	rows, err := s.db.Query(`SELECT id,doc_id,kind,content,embedding FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chunk
	for rows.Next() {
		var c Chunk
		var blob []byte
		if err := rows.Scan(&c.ID, &c.DocID, &c.Kind, &c.Content, &blob); err != nil {
			return nil, err
		}
		c.Embedding = decodeVec(blob)
		out = append(out, c)
	}
	return out, nil
}

// ChunkContents 某个资料的前 limit 个片段内容（资料预览用）
func (s *Store) ChunkContents(docID int64, limit int) ([]string, error) {
	rows, err := s.db.Query(`SELECT content FROM chunks WHERE doc_id=? ORDER BY id LIMIT ?`, docID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// EmbeddingStats 片段总数、还没有向量的片段数
func (s *Store) EmbeddingStats() (total, missing int) {
	_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(embedding IS NULL),0) FROM chunks`).Scan(&total, &missing)
	return
}

// UpdateEmbeddings 批量写入片段向量
func (s *Store) UpdateEmbeddings(cs []Chunk) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, c := range cs {
		if _, err := tx.Exec(`UPDATE chunks SET embedding=? WHERE id=?`, encodeVec(c.Embedding), c.ID); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ClearEmbeddings 清空所有向量（换了向量模型后旧向量作废）
func (s *Store) ClearEmbeddings() error {
	_, err := s.db.Exec(`UPDATE chunks SET embedding=NULL`)
	return err
}

// ClearVectorWarnings 向量补全后，去掉资料列表上“向量模型不可用”的提示
func (s *Store) ClearVectorWarnings() {
	_, _ = s.db.Exec(`UPDATE documents SET error='' WHERE status='done' AND error LIKE '%向量%'`)
}

// FirstDocOfKind 某类资料的第一条（如“示范回复”这种全局只有一条的文档）
func (s *Store) FirstDocOfKind(kind string) (int64, bool) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM documents WHERE kind=? ORDER BY id LIMIT 1`, kind).Scan(&id)
	return id, err == nil
}

// IncDocChunks 文档片段数 +1
func (s *Store) IncDocChunks(id int64) {
	_, _ = s.db.Exec(`UPDATE documents SET chunks=chunks+1 WHERE id=?`, id)
}

func encodeVec(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// ---------- customers & messages ----------

type Customer struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Notes       string `json:"notes"`
	Tags        string `json:"tags"`
	Profile     string `json:"profile"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	LastMessage string `json:"last_message,omitempty"`
	LastRole    string `json:"last_role,omitempty"` // customer 表示客户在等回复
}

func (s *Store) AddCustomer(c *Customer) error {
	t := now()
	res, err := s.db.Exec(`INSERT INTO customers(name,platform,notes,tags,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		c.Name, c.Platform, c.Notes, c.Tags, t, t)
	if err != nil {
		return err
	}
	c.ID, _ = res.LastInsertId()
	c.CreatedAt, c.UpdatedAt = t, t
	return nil
}

func (s *Store) UpdateCustomer(c *Customer) error {
	_, err := s.db.Exec(`UPDATE customers SET name=?,platform=?,notes=?,tags=?,updated_at=? WHERE id=?`,
		c.Name, c.Platform, c.Notes, c.Tags, now(), c.ID)
	return err
}

func (s *Store) SetProfile(id int64, profile string) error {
	_, err := s.db.Exec(`UPDATE customers SET profile=? WHERE id=?`, profile, id)
	return err
}

func (s *Store) TouchCustomer(id int64) {
	_, _ = s.db.Exec(`UPDATE customers SET updated_at=? WHERE id=?`, now(), id)
}

func (s *Store) DeleteCustomer(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM messages WHERE customer_id=?`, id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM customers WHERE id=?`, id)
	return err
}

func (s *Store) GetCustomer(id int64) (*Customer, error) {
	var c Customer
	err := s.db.QueryRow(`SELECT id,name,platform,notes,tags,profile,created_at,updated_at FROM customers WHERE id=?`, id).
		Scan(&c.ID, &c.Name, &c.Platform, &c.Notes, &c.Tags, &c.Profile, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) ListCustomers() ([]Customer, error) {
	rows, err := s.db.Query(`
SELECT c.id,c.name,c.platform,c.notes,c.tags,c.profile,c.created_at,c.updated_at,
  COALESCE((SELECT content FROM messages m WHERE m.customer_id=c.id ORDER BY m.id DESC LIMIT 1),''),
  COALESCE((SELECT role FROM messages m WHERE m.customer_id=c.id ORDER BY m.id DESC LIMIT 1),'')
FROM customers c ORDER BY c.updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Customer{}
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.Name, &c.Platform, &c.Notes, &c.Tags, &c.Profile, &c.CreatedAt, &c.UpdatedAt, &c.LastMessage, &c.LastRole); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

type Message struct {
	ID          int64  `json:"id"`
	CustomerID  int64  `json:"customer_id"`
	Role        string `json:"role"`
	Content     string `json:"content"`
	Translation string `json:"translation"`
	ImagePath   string `json:"image_path"`
	Analysis    string `json:"analysis"`
	AIReply     string `json:"ai_reply"`
	Action      string `json:"action"`
	ReplyTo     int64  `json:"reply_to"`
	CreatedAt   int64  `json:"created_at"`
	MediaIDs    string `json:"media_ids"`
}

const msgCols = `id,customer_id,role,content,translation,image_path,analysis,ai_reply,action,reply_to,created_at,media_ids`

func scanMsg(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	err := sc.Scan(&m.ID, &m.CustomerID, &m.Role, &m.Content, &m.Translation, &m.ImagePath, &m.Analysis,
		&m.AIReply, &m.Action, &m.ReplyTo, &m.CreatedAt, &m.MediaIDs)
	return m, err
}

func (s *Store) AddMessage(m *Message) error {
	m.CreatedAt = now()
	res, err := s.db.Exec(`INSERT INTO messages(customer_id,role,content,translation,image_path,analysis,ai_reply,action,reply_to,created_at,media_ids)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, m.CustomerID, m.Role, m.Content, m.Translation, m.ImagePath, m.Analysis, m.AIReply, m.Action, m.ReplyTo, m.CreatedAt, m.MediaIDs)
	if err != nil {
		return err
	}
	m.ID, _ = res.LastInsertId()
	s.TouchCustomer(m.CustomerID)
	return nil
}

func (s *Store) UpdateMessageAnalysis(id int64, translation, analysis string) {
	_, _ = s.db.Exec(`UPDATE messages SET translation=?, analysis=? WHERE id=?`, translation, analysis, id)
}

func (s *Store) GetMessage(id int64) (*Message, error) {
	m, err := scanMsg(s.db.QueryRow(`SELECT `+msgCols+` FROM messages WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) DeleteMessage(id int64) error {
	_, err := s.db.Exec(`DELETE FROM messages WHERE id=?`, id)
	return err
}

// Messages 返回某客户最近 limit 条消息（按时间正序）；limit<=0 表示全部
func (s *Store) Messages(customerID int64, limit int) ([]Message, error) {
	q := `SELECT ` + msgCols + ` FROM messages WHERE customer_id=? ORDER BY id DESC`
	args := []any{customerID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *Store) CountCustomerMessages(customerID int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE customer_id=? AND role='customer'`, customerID).Scan(&n)
	return n
}

type Stats struct {
	Customers int            `json:"customers"`
	Documents int            `json:"documents"`
	Snippets  int            `json:"snippets"`
	Media     int            `json:"media"`
	MediaSent int            `json:"media_sent"`
	Chunks    int            `json:"chunks"`
	Replies   int            `json:"replies"`
	Accepted  int            `json:"accepted"`
	Edited    int            `json:"edited"`
	Manual    int            `json:"manual"`
	Intents   map[string]int `json:"intents"`
}

func (s *Store) Stats() Stats {
	st := Stats{Intents: map[string]int{}}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM customers`).Scan(&st.Customers)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM documents WHERE kind IN ('doc','history')`).Scan(&st.Documents)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM documents WHERE kind='snippet'`).Scan(&st.Snippets)
	_ = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(send_count),0) FROM documents WHERE kind='media'`).Scan(&st.Media, &st.MediaSent)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&st.Chunks)
	_ = s.db.QueryRow(`SELECT COUNT(*),
  COALESCE(SUM(action='accepted'),0), COALESCE(SUM(action='edited'),0), COALESCE(SUM(action='manual'),0)
  FROM messages WHERE role='agent'`).Scan(&st.Replies, &st.Accepted, &st.Edited, &st.Manual)
	rows, err := s.db.Query(`SELECT COALESCE(json_extract(analysis,'$.intent'),'') AS i, COUNT(*) FROM messages
  WHERE role='customer' AND analysis<>'' GROUP BY i`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int
			_ = rows.Scan(&k, &n)
			if k != "" {
				st.Intents[k] = n
			}
		}
	}
	return st
}
