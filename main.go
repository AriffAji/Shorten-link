package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	_ "embed"
	"html/template"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	baseURL       = "http://localhost:8080"
	maxPerJam     = 10
	jumlahTerbaru = 10
	panjangAcak   = 6
	panjangToken  = 24
	alfabet       = "0123456789abcdefghijklmnopqrstuvwxyz"
)

//go:embed index.html
var indexHTML string

var (
	db       *sql.DB
	slugRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,60}$`)
	reserved = map[string]bool{
		"admin": true, "login": true, "logout": true, "static": true,
		"api": true, "dashboard": true, "buat": true, "favicon.ico": true,
		"ganti": true, "hapus": true,
	}
	errSlugTidakValid = errors.New("nama tautan tidak valid (gunakan huruf, angka, dan tanda hubung, 3-61 karakter, dan tidak boleh kata terlarang)")
	errSlugDipakai    = errors.New("nama tautan sudah dipakai, coba yang lain")
	pembatas          = &limiter{hits: map[string][]time.Time{}}
	tpl               = template.Must(template.New("halaman").Parse(indexHTML))
)

type link struct {
	Kode, Tujuan, Pendek, Dibuat string
	Klik                         int
	Bisa                         bool
}

type halamanData struct {
	Domain, Pesan, Tujuan, Slug, Pendek, Info, InfoTipe string
	Sukses                                              bool
	Links                                               []link
}

// ---------- Pembatas jumlah pembuatan per IP ----------

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	batas := now.Add(-time.Hour)
	var masih []time.Time
	for _, t := range l.hits[ip] {
		if t.After(batas) {
			masih = append(masih, t)
		}
	}
	if len(masih) >= maxPerJam {
		l.hits[ip] = masih
		return false
	}
	l.hits[ip] = append(masih, now)
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- Slug, kode acak, dan token ----------

func normalizeSlug(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Join(strings.Fields(s), "-")
	if !slugRE.MatchString(s) || reserved[s] {
		return "", false
	}
	return s, true
}

func buatKodeAcak(n int) string {
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alfabet))))
		if err != nil {
			log.Fatal(err)
		}
		b[i] = alfabet[idx.Int64()]
	}
	return string(b)
}

func milikSaya(r *http.Request, kode, token string) bool {
	if token == "" {
		return false
	}
	c, err := r.Cookie("kelola_" + kode)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(token)) == 1
}

// ---------- Validasi URL ----------

func validasiURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("URL tujuan harus diawali http:// atau https:// dan valid")
	}
	return s, nil
}

// ---------- Database ----------

func initDB() {
	var err error
	db, err = sql.Open("sqlite", "links.db")
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS links (
		kode TEXT PRIMARY KEY,
		url_tujuan TEXT NOT NULL,
		jumlah_klik INTEGER NOT NULL DEFAULT 0,
		dibuat_pada TEXT NOT NULL DEFAULT (datetime('now','localtime')),
		token_kelola TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		log.Fatal(err)
	}
}

func simpanLink(slugRaw, tujuan, token string) (string, error) {
	if strings.TrimSpace(slugRaw) != "" {
		kode, ok := normalizeSlug(slugRaw)
		if !ok {
			return "", errSlugTidakValid
		}
		if _, err := db.Exec(`INSERT INTO links (kode, url_tujuan, token_kelola) VALUES (?, ?, ?)`, kode, tujuan, token); err != nil {
			return "", errSlugDipakai
		}
		return kode, nil
	}
	for i := 0; i < 5; i++ {
		kode := buatKodeAcak(panjangAcak)
		if _, err := db.Exec(`INSERT INTO links (kode, url_tujuan, token_kelola) VALUES (?, ?, ?)`, kode, tujuan, token); err == nil {
			return kode, nil
		}
	}
	return "", errors.New("gagal membuat kode acak, coba lagi")
}

func ambilToken(kode string) (string, error) {
	var token string
	err := db.QueryRow(`SELECT token_kelola FROM links WHERE kode = ?`, kode).Scan(&token)
	return token, err
}

func linkTerbaru(r *http.Request) ([]link, error) {
	rows, err := db.Query(`SELECT kode, url_tujuan, jumlah_klik, dibuat_pada, token_kelola
		FROM links ORDER BY dibuat_pada DESC, rowid DESC LIMIT ?`, jumlahTerbaru)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hasil []link
	for rows.Next() {
		var l link
		var token string
		if err := rows.Scan(&l.Kode, &l.Tujuan, &l.Klik, &l.Dibuat, &token); err != nil {
			return nil, err
		}
		l.Pendek = baseURL + "/" + l.Kode
		l.Bisa = milikSaya(r, l.Kode, token)
		hasil = append(hasil, l)
	}
	return hasil, rows.Err()
}

// ---------- Handler ----------

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		redirectHandler(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data := halamanData{Domain: strings.TrimPrefix(baseURL, "http://")}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		data, status = prosesForm(w, r, data)
		if data.Sukses {
			http.Redirect(w, r, "/?tipe=sukses&info="+url.QueryEscape("Tautan berhasil dibuat: "+data.Pendek), http.StatusSeeOther)
			return
		}
	} else {
		data.Info = r.URL.Query().Get("info")
		data.InfoTipe = r.URL.Query().Get("tipe")
	}

	terbaru, err := linkTerbaru(r)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	data.Links = terbaru

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tpl.Execute(w, data); err != nil {
		log.Println("gagal render halaman:", err)
	}
}

func prosesForm(w http.ResponseWriter, r *http.Request, data halamanData) (halamanData, int) {
	data.Tujuan = r.FormValue("url")
	data.Slug = r.FormValue("slug")

	if !pembatas.allow(clientIP(r)) {
		data.Pesan = "Batas pembuatan tercapai (maksimal 10 tautan per jam). Coba lagi nanti."
		return data, http.StatusTooManyRequests
	}

	tujuan, err := validasiURL(data.Tujuan)
	if err != nil {
		data.Pesan = err.Error()
		return data, http.StatusBadRequest
	}

	token := buatKodeAcak(panjangToken)
	kode, err := simpanLink(data.Slug, tujuan, token)
	if err != nil {
		data.Pesan = err.Error()
		if errors.Is(err, errSlugTidakValid) || errors.Is(err, errSlugDipakai) {
			return data, http.StatusBadRequest
		}
		return data, http.StatusInternalServerError
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "kelola_" + kode,
		Value:    token,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	data.Sukses = true
	data.Pendek = baseURL + "/" + kode
	data.Tujuan = ""
	data.Slug = ""
	return data, http.StatusOK
}

func gantiHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kodeLama, ok := normalizeSlug(r.FormValue("kode"))
	if !ok {
		http.Error(w, "kode tidak valid", http.StatusBadRequest)
		return
	}
	token, err := ambilToken(kodeLama)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if !milikSaya(r, kodeLama, token) {
		http.Error(w, "kamu tidak punya akses ke tautan ini (hanya bisa dikelola dari browser yang membuatnya)", http.StatusForbidden)
		return
	}
	tujuan, err := validasiURL(r.FormValue("url"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	kodeBaru, ok := normalizeSlug(r.FormValue("slug"))
	if !ok {
		http.Error(w, errSlugTidakValid.Error(), http.StatusBadRequest)
		return
	}
	if kodeBaru != kodeLama {
		var ada int
		db.QueryRow(`SELECT COUNT(*) FROM links WHERE kode = ?`, kodeBaru).Scan(&ada)
		if ada > 0 {
			http.Error(w, errSlugDipakai.Error(), http.StatusConflict)
			return
		}
	}
	if _, err := db.Exec(`UPDATE links SET kode = ?, url_tujuan = ? WHERE kode = ?`, kodeBaru, tujuan, kodeLama); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	// Pindahkan cookie kelola ke nama tautan yang baru
	if kodeBaru != kodeLama {
		http.SetCookie(w, &http.Cookie{Name: "kelola_" + kodeLama, Value: "", Path: "/", MaxAge: -1})
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "kelola_" + kodeBaru,
		Value:    token,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/?tipe=sukses&info="+url.QueryEscape("Tautan berhasil diperbarui: "+baseURL+"/"+kodeBaru), http.StatusSeeOther)
}

func hapusHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kode, ok := normalizeSlug(r.FormValue("kode"))
	if !ok {
		http.Error(w, "kode tidak valid", http.StatusBadRequest)
		return
	}
	token, err := ambilToken(kode)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if !milikSaya(r, kode, token) {
		http.Error(w, "kamu tidak punya akses ke tautan ini (hanya bisa dikelola dari browser yang membuatnya)", http.StatusForbidden)
		return
	}
	if _, err := db.Exec(`DELETE FROM links WHERE kode = ?`, kode); err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "kelola_" + kode, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/?tipe=hapus&info="+url.QueryEscape("Tautan berhasil dihapus"), http.StatusSeeOther)
}

func redirectHandler(w http.ResponseWriter, r *http.Request) {
	kode, ok := normalizeSlug(strings.TrimPrefix(r.URL.Path, "/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	var tujuan string
	err := db.QueryRow(`SELECT url_tujuan FROM links WHERE kode = ?`, kode).Scan(&tujuan)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	db.Exec(`UPDATE links SET jumlah_klik = jumlah_klik + 1 WHERE kode = ?`, kode)
	http.Redirect(w, r, tujuan, http.StatusFound)
}

func main() {
	initDB()
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/ganti", gantiHandler)
	http.HandleFunc("/hapus", hapusHandler)
	log.Println("Jalan di", baseURL)
	log.Fatal(http.ListenAndServe(":8080", nil))
}