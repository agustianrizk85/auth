package http

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

/* ════════════════════════════════════════════════════════════════════════
 * Messaging (Chat) — direct messages + per-division channels + file attachments.
 *
 * Hosted in the auth service because every user and the dashboard's own token
 * already live here, so no new backend/port/bridge is needed. Two conversation
 * kinds share one store:
 *   • DM      — conv id = sorted(userA,userB); `to` set.
 *   • Channel — conv id = "chan:<deptCode>"; `to` empty (broadcast); membership
 *               = the caller holds a role in that department (directors/super = all).
 *
 * Realtime is a Server-Sent Events stream (revision fan-out). Storage is
 * in-memory for now (file bytes capped at 10 MiB); Postgres persistence is a
 * later phase.
 * ════════════════════════════════════════════════════════════════════════ */

const maxFileBytes = 10 << 20 // 10 MiB

type chatAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int    `json:"size"`
	Mime string `json:"mime"`
}

type chatMessage struct {
	ID        string          `json:"id"`
	Conv      string          `json:"conv"`
	From      string          `json:"from"` // sender user ID
	To        string          `json:"to"`   // recipient user ID ("" for a channel)
	Body      string          `json:"body"`
	CreatedAt int64           `json:"createdAt"` // unix millis
	Att       *chatAttachment `json:"att,omitempty"`
}

type chatStore struct {
	mu       sync.Mutex
	seq      int64
	rev      int64
	msgs     []chatMessage
	lastRead map[string]int64  // "convID|userID" -> unix millis of last read
	files    map[string][]byte // attachment ID -> bytes
	subs     map[chan int64]struct{}
}

func newChatStore() *chatStore {
	return &chatStore{lastRead: map[string]int64{}, files: map[string][]byte{}, subs: map[chan int64]struct{}{}}
}

// convID is the id of the DM conversation between two users (order-free).
func convID(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// channelConv is the id of a per-division channel conversation.
func channelConv(code string) string { return "chan:" + code }

func (s *chatStore) add(conv, from, to, body string, att *chatAttachment) chatMessage {
	s.mu.Lock()
	s.seq++
	m := chatMessage{
		ID:        strconv.FormatInt(s.seq, 10),
		Conv:      conv,
		From:      from,
		To:        to,
		Body:      body,
		CreatedAt: time.Now().UnixMilli(),
		Att:       att,
	}
	s.msgs = append(s.msgs, m)
	s.rev++
	rev := s.rev
	s.mu.Unlock()
	s.notify(rev)
	return m
}

func (s *chatStore) send(from, to, body string, att *chatAttachment) chatMessage {
	return s.add(convID(from, to), from, to, body, att)
}
func (s *chatStore) postChannel(code, from, body string, att *chatAttachment) chatMessage {
	return s.add(channelConv(code), from, "", body, att)
}

func (s *chatStore) byConv(conv string) []chatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []chatMessage{}
	for _, m := range s.msgs {
		if m.Conv == conv {
			out = append(out, m)
		}
	}
	return out
}

func (s *chatStore) markReadConv(conv, user string) {
	s.mu.Lock()
	s.lastRead[conv+"|"+user] = time.Now().UnixMilli()
	s.rev++
	rev := s.rev
	s.mu.Unlock()
	s.notify(rev)
}

func (s *chatStore) storeFile(data []byte) string {
	s.mu.Lock()
	s.seq++
	id := "f" + strconv.FormatInt(s.seq, 10)
	s.files[id] = data
	s.mu.Unlock()
	return id
}
func (s *chatStore) getFile(id string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.files[id]
	return d, ok
}
func (s *chatStore) messageByAtt(attID string) (chatMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if m.Att != nil && m.Att.ID == attID {
			return m, true
		}
	}
	return chatMessage{}, false
}

// summary of a DM message body for a conversation-list preview.
func preview(m chatMessage) string {
	if m.Att != nil && strings.TrimSpace(m.Body) == "" {
		return "📎 " + m.Att.Name
	}
	return m.Body
}

type convSummary struct {
	UserID string `json:"userId"`
	Last   string `json:"last"`
	LastAt int64  `json:"lastAt"`
	Unread int    `json:"unread"`
}

func (s *chatStore) conversations(user string) []convSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	type agg struct {
		last   string
		lastAt int64
		unread int
	}
	byOther := map[string]*agg{}
	for _, msg := range s.msgs {
		if msg.To == "" { // channel message — not a DM conversation
			continue
		}
		var other string
		switch {
		case msg.From == user:
			other = msg.To
		case msg.To == user:
			other = msg.From
		default:
			continue
		}
		a := byOther[other]
		if a == nil {
			a = &agg{}
			byOther[other] = a
		}
		if msg.CreatedAt >= a.lastAt {
			a.last = preview(msg)
			a.lastAt = msg.CreatedAt
		}
		if msg.To == user && msg.CreatedAt > s.lastRead[convID(user, other)+"|"+user] {
			a.unread++
		}
	}
	out := make([]convSummary, 0, len(byOther))
	for other, a := range byOther {
		out = append(out, convSummary{UserID: other, Last: a.last, LastAt: a.lastAt, Unread: a.unread})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt > out[j].LastAt })
	return out
}

func (s *chatStore) dmUnread(user string) int {
	n := 0
	for _, c := range s.conversations(user) {
		n += c.Unread
	}
	return n
}

// channelStat returns the last-message preview + unread count for a channel.
func (s *chatStore) channelStat(code, user string) (last string, lastAt int64, unread int) {
	conv := channelConv(code)
	s.mu.Lock()
	defer s.mu.Unlock()
	lr := s.lastRead[conv+"|"+user]
	for _, m := range s.msgs {
		if m.Conv != conv {
			continue
		}
		if m.CreatedAt >= lastAt {
			last = preview(m)
			lastAt = m.CreatedAt
		}
		if m.From != user && m.CreatedAt > lr {
			unread++
		}
	}
	return last, lastAt, unread
}

/* ── SSE fan-out ─────────────────────────────────────────────────────────── */

func (s *chatStore) subscribe() chan int64 {
	ch := make(chan int64, 8)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}
func (s *chatStore) unsubscribe(ch chan int64) {
	s.mu.Lock()
	delete(s.subs, ch)
	s.mu.Unlock()
}
func (s *chatStore) notify(rev int64) {
	s.mu.Lock()
	for ch := range s.subs {
		select {
		case ch <- rev:
		default:
		}
	}
	s.mu.Unlock()
}
func (s *chatStore) currentRev() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

/* ── Directory / access helpers ──────────────────────────────────────────── */

type dirEntry struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

func (h *Handler) userDir(r *http.Request) map[string]dirEntry {
	m := map[string]dirEntry{}
	users, err := h.users.List(r.Context())
	if err != nil {
		return m
	}
	for _, u := range users {
		m[u.ID] = dirEntry{ID: u.ID, Username: u.Username, Name: u.Name}
	}
	return m
}

// callerChannels returns the department codes the caller may use as channels:
// every department they hold a role in (directors hold all); super users get
// every department in the catalogue.
func (h *Handler) callerChannels(r *http.Request) []string {
	c := claimsOf(r)
	codes := []string{}
	if c.Super {
		depts, err := h.users.Departments(r.Context())
		if err == nil {
			for _, d := range depts {
				codes = append(codes, d.Code)
			}
		}
		return codes
	}
	for code := range c.Roles {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

func (h *Handler) canAccessChannel(r *http.Request, code string) bool {
	c := claimsOf(r)
	if c.Super {
		return true
	}
	_, ok := c.Roles[code]
	return ok
}

/* ── Enriched message output (adds sender name + a `mine` flag) ───────────── */

type chatMessageOut struct {
	ID        string          `json:"id"`
	From      string          `json:"from"`
	FromName  string          `json:"fromName"`
	Body      string          `json:"body"`
	CreatedAt int64           `json:"createdAt"`
	Mine      bool            `json:"mine"`
	Att       *chatAttachment `json:"att,omitempty"`
}

func (h *Handler) enrich(r *http.Request, me string, msgs []chatMessage) []chatMessageOut {
	dir := h.userDir(r)
	out := make([]chatMessageOut, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, chatMessageOut{
			ID: m.ID, From: m.From, FromName: dir[m.From].Name,
			Body: m.Body, CreatedAt: m.CreatedAt, Mine: m.From == me, Att: m.Att,
		})
	}
	return out
}

/* ── HTTP handlers: directory + DM ───────────────────────────────────────── */

func (h *Handler) chatUsers(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	users, err := h.users.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []dirEntry{}
	for _, u := range users {
		if u.ID == me || !u.Active {
			continue
		}
		out = append(out, dirEntry{ID: u.ID, Username: u.Username, Name: u.Name})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) chatConversations(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	dir := h.userDir(r)
	type row struct {
		UserID   string `json:"userId"`
		Name     string `json:"name"`
		Username string `json:"username"`
		Last     string `json:"last"`
		LastAt   int64  `json:"lastAt"`
		Unread   int    `json:"unread"`
	}
	out := []row{}
	for _, c := range h.chat.conversations(me) {
		d := dir[c.UserID]
		out = append(out, row{UserID: c.UserID, Name: d.Name, Username: d.Username, Last: c.Last, LastAt: c.LastAt, Unread: c.Unread})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) chatThread(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	other := r.PathValue("userId")
	writeJSON(w, http.StatusOK, h.enrich(r, me, h.chat.byConv(convID(me, other))))
}

func (h *Handler) chatSend(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	other := r.PathValue("userId")
	req, ok := decode[struct {
		Body string `json:"body"`
	}](w, r)
	if !ok {
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeError(w, http.StatusBadRequest, "pesan kosong")
		return
	}
	if len(body) > 8000 {
		body = body[:8000]
	}
	if other == "" || other == me {
		writeError(w, http.StatusBadRequest, "penerima tidak valid")
		return
	}
	writeJSON(w, http.StatusCreated, h.chat.send(me, other, body, nil))
}

// chatSendFile — POST /api/messages/with/{userId}/file (multipart: file [+ body]).
func (h *Handler) chatSendFile(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	other := r.PathValue("userId")
	if other == "" || other == me {
		writeError(w, http.StatusBadRequest, "penerima tidak valid")
		return
	}
	att, body, ok := h.readUpload(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, h.chat.send(me, other, body, att))
}

func (h *Handler) chatRead(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	h.chat.markReadConv(convID(me, r.PathValue("userId")), me)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

/* ── HTTP handlers: channels ─────────────────────────────────────────────── */

func (h *Handler) chatChannels(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	names := map[string]string{}
	if depts, err := h.users.Departments(r.Context()); err == nil {
		for _, d := range depts {
			names[d.Code] = d.Name
		}
	}
	type row struct {
		Code   string `json:"code"`
		Name   string `json:"name"`
		Last   string `json:"last"`
		LastAt int64  `json:"lastAt"`
		Unread int    `json:"unread"`
	}
	out := []row{}
	for _, code := range h.callerChannels(r) {
		last, lastAt, unread := h.chat.channelStat(code, me)
		name := names[code]
		if name == "" {
			name = code
		}
		out = append(out, row{Code: code, Name: name, Last: last, LastAt: lastAt, Unread: unread})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) chatChannelThread(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !h.canAccessChannel(r, code) {
		writeError(w, http.StatusForbidden, "bukan anggota divisi")
		return
	}
	writeJSON(w, http.StatusOK, h.enrich(r, claimsOf(r).Subject, h.chat.byConv(channelConv(code))))
}

func (h *Handler) chatChannelSend(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !h.canAccessChannel(r, code) {
		writeError(w, http.StatusForbidden, "bukan anggota divisi")
		return
	}
	req, ok := decode[struct {
		Body string `json:"body"`
	}](w, r)
	if !ok {
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeError(w, http.StatusBadRequest, "pesan kosong")
		return
	}
	if len(body) > 8000 {
		body = body[:8000]
	}
	writeJSON(w, http.StatusCreated, h.chat.postChannel(code, claimsOf(r).Subject, body, nil))
}

func (h *Handler) chatChannelSendFile(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !h.canAccessChannel(r, code) {
		writeError(w, http.StatusForbidden, "bukan anggota divisi")
		return
	}
	att, body, ok := h.readUpload(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, h.chat.postChannel(code, claimsOf(r).Subject, body, att))
}

func (h *Handler) chatChannelRead(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	if !h.canAccessChannel(r, code) {
		writeError(w, http.StatusForbidden, "bukan anggota divisi")
		return
	}
	h.chat.markReadConv(channelConv(code), claimsOf(r).Subject)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

/* ── HTTP handlers: unread total, files, SSE ─────────────────────────────── */

// chatUnread — total unread across DMs + the caller's channels (sidebar badge).
func (h *Handler) chatUnread(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	total := h.chat.dmUnread(me)
	for _, code := range h.callerChannels(r) {
		_, _, u := h.chat.channelStat(code, me)
		total += u
	}
	writeJSON(w, http.StatusOK, map[string]int{"count": total})
}

// readUpload parses a multipart upload (field "file", optional "body"), stores
// the bytes, and returns the attachment + trimmed caption. On error it writes
// the response and returns ok=false.
func (h *Handler) readUpload(w http.ResponseWriter, r *http.Request) (*chatAttachment, string, bool) {
	if err := r.ParseMultipartForm(maxFileBytes + (1 << 20)); err != nil {
		writeError(w, http.StatusBadRequest, "upload gagal")
		return nil, "", false
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file tidak ditemukan")
		return nil, "", false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil || len(data) == 0 {
		writeError(w, http.StatusBadRequest, "file kosong")
		return nil, "", false
	}
	if len(data) > maxFileBytes {
		writeError(w, http.StatusBadRequest, "file terlalu besar (maks 10MB)")
		return nil, "", false
	}
	mime := fh.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}
	att := &chatAttachment{ID: h.chat.storeFile(data), Name: fh.Filename, Size: len(data), Mime: mime}
	return att, strings.TrimSpace(r.FormValue("body")), true
}

// chatFile — GET /api/messages/file/{attId} — download an attachment (only a
// participant of its DM, or a member of its channel, may fetch it).
func (h *Handler) chatFile(w http.ResponseWriter, r *http.Request) {
	me := claimsOf(r).Subject
	id := r.PathValue("attId")
	m, ok := h.chat.messageByAtt(id)
	if !ok || m.Att == nil {
		writeError(w, http.StatusNotFound, "lampiran tidak ditemukan")
		return
	}
	allowed := false
	if m.To == "" { // channel
		allowed = h.canAccessChannel(r, strings.TrimPrefix(m.Conv, "chan:"))
	} else {
		allowed = me == m.From || me == m.To
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "tidak diizinkan")
		return
	}
	data, ok := h.chat.getFile(id)
	if !ok {
		writeError(w, http.StatusNotFound, "berkas tidak ditemukan")
		return
	}
	w.Header().Set("Content-Type", m.Att.Mime)
	w.Header().Set("Content-Disposition", "inline; filename=\""+m.Att.Name+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// chatStream — GET /api/messages/stream?token=… — Server-Sent Events realtime.
func (h *Handler) chatStream(w http.ResponseWriter, r *http.Request) {
	if _, err := h.auth.Verify(r.URL.Query().Get("token")); err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := h.chat.subscribe()
	defer h.chat.unsubscribe(ch)
	fmt.Fprintf(w, "data: {\"rev\":%d}\n\n", h.chat.currentRev())
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case rev := <-ch:
			fmt.Fprintf(w, "data: {\"rev\":%d}\n\n", rev)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}
