package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

//go:embed templates/*.html
var tplFS embed.FS

var (
	usernameRe   = regexp.MustCompile(`^[A-Za-z0-9_]{3,30}$`)
	phoneRe      = regexp.MustCompile(`^[0-9+\- ]{8,15}$`)
	ict          = time.FixedZone("ICT", 7*3600)
	dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password"), bcrypt.DefaultCost)
)

type User struct {
	ID       int64
	Username string
	Role     string
}

type Page struct {
	User  *User
	CSRF  string
	Flash bool
	Error string
	Data  any
}

type Reservation struct {
	ID        int64
	UserID    int64
	Username  string
	Name      string
	Phone     string
	PartySize int
	When      string
	Note      string
}

type MenuItem struct {
	ID          int64
	Name        string
	Description string
	Price       float64
	Category    string
	ImageURL    string
}

type Review struct {
	ID        int64
	UserID    int64
	Username  string
	Rating    int
	Comment   string
	CreatedAt string
}

// DB ห่อ *sql.DB เพื่อรองรับทั้ง SQLite (ในเครื่อง) และ PostgreSQL (บนโฮส)
// โดยแปลง placeholder ? เป็น \$1,\$2,... อัตโนมัติเมื่อใช้ PostgreSQL
type DB struct {
	*sql.DB
	pg bool
}

func (d *DB) rebind(q string) string {
	if !d.pg {
		return q
	}
	var b strings.Builder
	n := 0
	for _, c := range q {
		if c == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func (d *DB) Exec(q string, a ...any) (sql.Result, error) { return d.DB.Exec(d.rebind(q), a...) }
func (d *DB) Query(q string, a ...any) (*sql.Rows, error) { return d.DB.Query(d.rebind(q), a...) }
func (d *DB) QueryRow(q string, a ...any) *sql.Row        { return d.DB.QueryRow(d.rebind(q), a...) }

type App struct {
	db    *DB
	pages map[string]*template.Template
	mu    sync.Mutex
	fails map[string][]time.Time
}

const schemaSQLite = `
CREATE TABLE IF NOT EXISTS users(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT UNIQUE NOT NULL,
  email TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'customer',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS sessions(
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS menu_items(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
  price REAL NOT NULL, category TEXT NOT NULL,
  image_url TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS reservations(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL, phone TEXT NOT NULL, party_size INTEGER NOT NULL,
  when_at TEXT NOT NULL, note TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS reviews(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  rating INTEGER NOT NULL, comment TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS orders(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  total_price REAL NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS order_items(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  menu_item_id INTEGER NOT NULL REFERENCES menu_items(id),
  quantity INTEGER NOT NULL,
  price REAL NOT NULL);
`

const pgNow = "(to_char(now() AT TIME ZONE 'Asia/Bangkok','YYYY-MM-DD HH24:MI'))"

const schemaPG = `
CREATE TABLE IF NOT EXISTS users(
  id BIGSERIAL PRIMARY KEY,
  username TEXT UNIQUE NOT NULL,
  email TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT 'customer',
  created_at TEXT NOT NULL DEFAULT ` + pgNow + `);
CREATE TABLE IF NOT EXISTS sessions(
  token TEXT PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at BIGINT NOT NULL);
CREATE TABLE IF NOT EXISTS menu_items(
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '',
  price DOUBLE PRECISION NOT NULL, category TEXT NOT NULL,
  image_url TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS reservations(
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name TEXT NOT NULL, phone TEXT NOT NULL, party_size INTEGER NOT NULL,
  when_at TEXT NOT NULL, note TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT ` + pgNow + `);
CREATE TABLE IF NOT EXISTS reviews(
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  rating INTEGER NOT NULL, comment TEXT NOT NULL,
  created_at TEXT NOT NULL DEFAULT ` + pgNow + `);
CREATE TABLE IF NOT EXISTS orders(
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  total_price DOUBLE PRECISION NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  created_at TEXT NOT NULL DEFAULT ` + pgNow + `);
CREATE TABLE IF NOT EXISTS order_items(
  id BIGSERIAL PRIMARY KEY,
  order_id BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  menu_item_id BIGINT NOT NULL REFERENCES menu_items(id),
  quantity INTEGER NOT NULL,
  price DOUBLE PRECISION NOT NULL);
`

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func token() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	var db *DB
	schema := schemaSQLite
	if url := os.Getenv("DATABASE_URL"); url != "" {
		raw, err := sql.Open("pgx", url)
		if err != nil {
			log.Fatal(err)
		}
		raw.SetMaxOpenConns(10)
		db = &DB{DB: raw, pg: true}
		schema = schemaPG
		log.Println("ใช้ฐานข้อมูล PostgreSQL")
	} else {
		raw, err := sql.Open("sqlite", env("DB_PATH", "restaurant.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
		if err != nil {
			log.Fatal(err)
		}
		raw.SetMaxOpenConns(1)
		db = &DB{DB: raw}
		log.Println("ใช้ฐานข้อมูล SQLite (ไฟล์)")
	}
	app := &App{db: db, fails: map[string][]time.Time{}, pages: map[string]*template.Template{}}
	for _, stmt := range strings.Split(schema, ");") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt + ")"); err != nil {
			log.Fatal("migrate: ", err)
		}
	}
	// เพิ่มคอลัมน์ image_url ให้ฐานข้อมูลเก่า (ถ้ามีอยู่แล้วจะ error ซึ่งไม่เป็นไร)
	if db.pg {
		db.Exec(`ALTER TABLE menu_items ADD COLUMN IF NOT EXISTS image_url TEXT NOT NULL DEFAULT ''`)
	} else {
		db.Exec(`ALTER TABLE menu_items ADD COLUMN image_url TEXT NOT NULL DEFAULT ''`)
	}
	for name, img := range map[string]string{
		"ผัดกะเพราหมูสับไข่ดาว": "/static/กระเพรา.jpg",
		"ต้มยำกุ้งน้ำข้น":       "/static/ต้มยำกุ้งน้ำข้น.jpg",
		"ส้มตำไทย":              "/static/ส้มตำไทย.jpg",
		"ชาไทยเย็น":             "/static/images.jpg",
		"ชาเขียว":               "/static/ชาเขียว.jpg",
		"ชาเขียวเย็น":           "/static/ชาเขียว.jpg",
		"ข้าวผัดหมู":            "/static/ข้าวผัดหมู.jpg",
		"ข้าวไข่ข้นกุ้ง":        "/static/ไข่ข้นกุ้ง.jpg",
		"ข้าวไข่ข้นหมูสับ":      "/static/ไข่ข้นหมูสับ.jpg",
		"ต้มแซ่บกระดูกหมู":      "/static/ต้มแซ่บกระดูกหมู.jpg",
		"ตำทะเล":                "/static/ตำทะเล.jpg",
		"ตำป่า":                 "/static/ตำป่า.jpg",
		"ยำวุ้นเส้นหมูยอ":       "/static/ยำวุ้นเส้นหมูยอ.jpg",
	} {
		db.Exec(`UPDATE menu_items SET image_url=? WHERE name=? AND image_url=''`, img, name)
	}
	// คอลัมน์เพิ่มของตาราง orders: ประเภทการสั่ง / การจองที่ผูกไว้ / หมายเหตุ
	ifNot := ""
	if db.pg {
		ifNot = "IF NOT EXISTS "
	}
	for _, q := range []string{
		"ALTER TABLE orders ADD COLUMN " + ifNot + "order_type TEXT NOT NULL DEFAULT 'normal'",
		"ALTER TABLE orders ADD COLUMN " + ifNot + "reservation_id " + map[bool]string{true: "BIGINT", false: "INTEGER"}[db.pg] + " REFERENCES reservations(id) ON DELETE SET NULL",
		"ALTER TABLE orders ADD COLUMN " + ifNot + "note TEXT NOT NULL DEFAULT ''",
	} {
		db.Exec(q) // ถ้ามีคอลัมน์อยู่แล้วจะ error ซึ่งไม่เป็นไร
	}
	app.seed()
	funcs := template.FuncMap{
		"dt":    func(s string) string { return strings.Replace(s, " ", "T", 1) },
		"stars": func(n int) string { return strings.Repeat("★", n) + strings.Repeat("☆", 5-n) },
		"statusTH": func(x string) string {
			return map[string]string{"pending": "รอดำเนินการ", "preparing": "กำลังทำ", "done": "เสร็จแล้ว", "cancelled": "ยกเลิก"}[x]
		},
		"typeTH": func(x string) string {
			if x == "reservation" {
				return "สั่งล่วงหน้าพร้อมจองโต๊ะ"
			}
			return "สั่งอาหารปกติ"
		},
	}
	for _, n := range []string{"home", "login", "register", "dashboard", "reservation_form", "reviews", "admin_menu", "order_form", "orders"} {
		app.pages[n] = template.Must(template.New("").Funcs(funcs).ParseFS(tplFS, "templates/layout.html", "templates/"+n+".html"))
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("GET /{$}", app.home)
	mux.HandleFunc("GET /register", app.registerForm)
	mux.HandleFunc("POST /register", app.register)
	mux.HandleFunc("GET /login", app.loginForm)
	mux.HandleFunc("POST /login", app.login)
	mux.HandleFunc("POST /logout", app.logout)
	mux.HandleFunc("GET /dashboard", app.auth(app.dashboard))
	mux.HandleFunc("GET /reservations/new", app.auth(app.resNew))
	mux.HandleFunc("POST /reservations", app.auth(app.resCreate))
	mux.HandleFunc("GET /reservations/{id}/edit", app.auth(app.resEdit))
	mux.HandleFunc("POST /reservations/{id}/edit", app.auth(app.resUpdate))
	mux.HandleFunc("POST /reservations/{id}/delete", app.auth(app.resDelete))
	mux.HandleFunc("GET /reviews", app.reviews)
	mux.HandleFunc("POST /reviews", app.auth(app.reviewCreate))
	mux.HandleFunc("POST /reviews/{id}/delete", app.auth(app.reviewDelete))
	mux.HandleFunc("GET /order", app.auth(app.orderForm))
	mux.HandleFunc("POST /orders", app.auth(app.orderCreate))
	mux.HandleFunc("GET /my-orders", app.auth(app.orderHistory))
	mux.HandleFunc("POST /orders/{id}/cancel", app.auth(app.orderCancel))
	mux.HandleFunc("POST /admin/orders/{id}/status", app.admin(app.orderStatus))
	mux.HandleFunc("GET /admin/menu", app.admin(app.menuList))
	mux.HandleFunc("POST /admin/menu", app.admin(app.menuCreate))
	mux.HandleFunc("POST /admin/menu/{id}/delete", app.admin(app.menuDelete))
	mux.HandleFunc("POST /admin/menu/{id}/image", app.admin(app.menuImage))

	srv := &http.Server{
		Addr: ":" + env("PORT", "8080"), Handler: app.secure(mux),
		ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	log.Println("listening on", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

func (a *App) seed() {
	var n int
	a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n)
	if n == 0 {
		u, p := env("ADMIN_USER", "admin"), env("ADMIN_PASS", "ChangeMe123!")
		h, _ := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
		a.db.Exec(`INSERT INTO users(username,email,password_hash,role) VALUES(?,?,?,'admin')`, u, "admin@example.com", string(h))
		log.Println("สร้างผู้ดูแลระบบ:", u, "(เปลี่ยนรหัสผ่านผ่าน ADMIN_PASS ก่อนใช้งานจริง)")
	}
	a.db.QueryRow(`SELECT COUNT(*) FROM menu_items`).Scan(&n)
	if n == 0 {
		for _, m := range []MenuItem{
			{Name: "ผัดกะเพราหมูสับไข่ดาว", Description: "เผ็ดร้อนหอมใบกะเพรา", Price: 70, Category: "จานเดียว", ImageURL: "/static/กระเพรา.jpg"},
			{Name: "ต้มยำกุ้งน้ำข้น", Description: "กุ้งแม่น้ำตัวโต เปรี้ยวเผ็ดกลมกล่อม", Price: 220, Category: "ต้ม/แกง", ImageURL: "/static/ต้มยำกุ้งน้ำข้น.jpg"},
			{Name: "ส้มตำไทย", Description: "รสจัดจ้าน", Price: 60, Category: "ยำ/ตำ", ImageURL: "/static/ส้มตำไทย.jpg"},
			{Name: "ชาไทยเย็น", Description: "หอมมัน หวานน้อยได้", Price: 45, Category: "เครื่องดื่ม", ImageURL: "/static/images.jpg"},
			{Name: "ชาเขียวเย็น", Description: "ชาเขียวหอมเข้มข้น รสชาตินุ่มละมุน หวานมันกำลังดี ดื่มแล้วสดชื่นกระปรี้กระเปร่า", Price: 45, Category: "เครื่องดื่ม", ImageURL: "/static/ชาเขียว.jpg"},
			{Name: "ข้าวผัดหมู", Description: "ข้าวผัดหมูแสนอร่อย", Price: 45, Category: "จานเดียว", ImageURL: "/static/ข้าวผัดหมู.jpg"},
			{Name: "ข้าวไข่ข้นกุ้ง", Description: "ไข่ข้นนุ่มละมุนลิ้น รสชาติกลมกล่อม ท็อปด้วยกุ้งสดเนื้อเด้งสุกกำลังดี", Price: 45, Category: "จานเดียว", ImageURL: "/static/ไข่ข้นกุ้ง.jpg"},
			{Name: "ข้าวไข่ข้นหมูสับ", Description: "ไข่ข้นเนื้อนุ่มละมุน และหมูสับผัดกลมกล่อม หอมอร่อยเข้ากันได้อย่างลงตัว", Price: 45, Category: "จานเดียว", ImageURL: "/static/ไข่ข้นหมูสับ.jpg"},
			{Name: "ต้มแซ่บกระดูกหมู", Description: "ต้มแซ่บรสจัดจ้าน เปรี้ยว เผ็ด เค็มกลมกล่อม หอมกลิ่นสมุนไพร เคี่ยวพร้อมกระดูกหมูเนื้อนุ่มเปื่อยกำลังดี", Price: 45, Category: "ต้ม/แกง", ImageURL: "/static/ต้มแซ่บกระดูกหมู.jpg"},
			{Name: "ตำทะเล", Description: "ส้มตำรสเด็ดจัดจ้าน คลุกเคล้าวัตถุดิบทะเลสดใหม่ ทั้งกุ้ง ปลาหมึก เนื้อเด้ง รสชาติเปรี้ยว เผ็ด แซ่บถึงใจ", Price: 45, Category: "ยำ/ตำ", ImageURL: "/static/ตำทะเล.jpg"},
			{Name: "ตำป่า", Description: "ส้มตำปลาร้าเข้มข้นรสจัดจ้าน เครื่องแน่นจัดเต็ม แซ่บนัวถึงใจ", Price: 45, Category: "ยำ/ตำ", ImageURL: "/static/ตำป่า.jpg"},
			{Name: "ยำวุ้นเส้นหมูยอ", Description: "วุ้นเส้นเหนียวนุ่ม ยำคลุกเคล้ากับหมูยออย่างดี น้ำยำรสชาติจัดจ้าน", Price: 45, Category: "ยำ/ตำ", ImageURL: "/static/ยำวุ้นเส้นหมูยอ.jpg"},
			{Name: "โกโก้เย็น", Description: "โกโก้เข้มข้น หอมมัน รสชาติหวานกลมกล่อมกำลังดี ดื่มแล้วสดชื่นชื่นใจ", Price: 45, Category: "เครื่องดื่ม", ImageURL: "/static/โกโก้เย็น.jpg"},
		} {
			a.db.Exec(`INSERT INTO menu_items(name,description,price,category,image_url) VALUES(?,?,?,?,?)`, m.Name, m.Description, m.Price, m.Category, m.ImageURL)
		}
	}
}

// ---------- ความปลอดภัย ----------

func (a *App) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self' 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if r.Method == http.MethodPost {
			c, err := r.Cookie("csrf")
			if err != nil || r.ParseForm() != nil ||
				subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("csrf"))) != 1 {
				http.Error(w, "CSRF token ไม่ถูกต้อง", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func secureCookie() bool { return os.Getenv("COOKIE_SECURE") == "1" }

func (a *App) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie("csrf"); err == nil && len(c.Value) == 64 {
		return c.Value
	}
	t := token()
	http.SetCookie(w, &http.Cookie{Name: "csrf", Value: t, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureCookie()})
	return t
}

func (a *App) currentUser(r *http.Request) *User {
	c, err := r.Cookie("sid")
	if err != nil {
		return nil
	}
	u := &User{}
	err = a.db.QueryRow(`SELECT u.id,u.username,u.role FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token=? AND s.expires_at>?`, c.Value, time.Now().Unix()).Scan(&u.ID, &u.Username, &u.Role)
	if err != nil {
		return nil
	}
	return u
}

func (a *App) auth(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := a.currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		h(w, r, u)
	}
}

func (a *App) admin(h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return a.auth(func(w http.ResponseWriter, r *http.Request, u *User) {
		if u.Role != "admin" {
			http.Error(w, "เฉพาะผู้ดูแลระบบ", http.StatusForbidden)
			return
		}
		h(w, r, u)
	})
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// จำกัดล็อกอินผิดไม่เกิน 5 ครั้งใน 10 นาที ต่อ IP
func (a *App) tooMany(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	var keep []time.Time
	for _, t := range a.fails[ip] {
		if time.Since(t) < 10*time.Minute {
			keep = append(keep, t)
		}
	}
	a.fails[ip] = keep
	return len(keep) >= 5
}

func (a *App) addFail(ip string) {
	a.mu.Lock()
	a.fails[ip] = append(a.fails[ip], time.Now())
	a.mu.Unlock()
}

// ---------- ตัวช่วยแสดงผล ----------

func (a *App) page(w http.ResponseWriter, r *http.Request) Page {
	return Page{User: a.currentUser(r), CSRF: a.csrfToken(w, r), Flash: r.URL.Query().Get("ok") == "1"}
}

func (a *App) render(w http.ResponseWriter, name string, p Page, status int) {
	var buf bytes.Buffer
	if err := a.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		log.Println("template error:", name, err)
		http.Error(w, "แสดงหน้า "+name+" ไม่สำเร็จ (ดู log ของเซิร์ฟเวอร์)", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (a *App) serverErr(w http.ResponseWriter, err error) {
	log.Println("error:", err)
	http.Error(w, "เกิดข้อผิดพลาดภายในระบบ", http.StatusInternalServerError)
}

// ---------- หน้าแรก / เมนู ----------

func (a *App) menuItems() []MenuItem {
	rows, err := a.db.Query(`SELECT id,name,description,price,category,COALESCE(image_url,'') FROM menu_items ORDER BY category,name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []MenuItem
	for rows.Next() {
		var m MenuItem
		rows.Scan(&m.ID, &m.Name, &m.Description, &m.Price, &m.Category, &m.ImageURL)
		out = append(out, m)
	}
	return out
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	p := a.page(w, r)
	p.Data = a.menuItems()
	a.render(w, "home", p, 200)
}

// ---------- สั่งอาหาร ----------

type OrderItemView struct {
	Name  string
	Qty   int
	Price float64
}

type OrderView struct {
	ID                                         int64
	Username, Type, ResWhen, Status, CreatedAt string
	Note                                       string
	Total                                      float64
	Items                                      []OrderItemView
}

type orderFormData struct {
	Menu         []MenuItem
	Reservations []Reservation
	Selected     int64
	Type         string
	NewRes       bool
}

func (a *App) renderOrderForm(w http.ResponseWriter, r *http.Request, u *User, msg string, status int) {
	p := a.page(w, r)
	p.Error = msg
	sel, _ := strconv.ParseInt(r.FormValue("reservation_id"), 10, 64)
	if sel == 0 {
		sel, _ = strconv.ParseInt(r.FormValue("reservation"), 10, 64)
	}
	d := orderFormData{Menu: a.menuItems(), Selected: sel, NewRes: r.FormValue("new") == "1"}
	now := time.Now().In(ict).Format("2006-01-02 15:04")
	for _, x := range a.listRes(u.ID) {
		if x.When >= now || x.ID == sel {
			d.Reservations = append(d.Reservations, x)
		}
	}
	if sel != 0 || r.FormValue("order_type") == "reservation" {
		d.Type = "reservation"
	}
	p.Data = d
	a.render(w, "order_form", p, status)
}

func (a *App) orderForm(w http.ResponseWriter, r *http.Request, u *User) {
	a.renderOrderForm(w, r, u, "", http.StatusOK)
}

func (a *App) orderCreate(w http.ResponseWriter, r *http.Request, u *User) {
	prices := map[int64]float64{}
	menu := a.menuItems()
	for _, m := range menu {
		prices[m.ID] = m.Price
	}

	items := map[int64]int{}
	if mid := r.PostFormValue("menu_id"); mid != "" { // สั่งด่วนจากหน้าแรก (1 รายการ)
		id, _ := strconv.ParseInt(mid, 10, 64)
		q, _ := strconv.Atoi(r.PostFormValue("quantity"))
		if _, ok := prices[id]; !ok {
			http.Error(w, "ไม่พบเมนูนี้", http.StatusBadRequest)
			return
		}
		if q < 1 {
			q = 1
		}
		items[id] = q
	} else { // ฟอร์มสั่งหลายรายการ: qty_<id>
		for _, m := range menu {
			q, _ := strconv.Atoi(r.PostFormValue("qty_" + strconv.FormatInt(m.ID, 10)))
			if q > 0 {
				items[m.ID] = q
			}
		}
	}
	if len(items) == 0 {
		a.renderOrderForm(w, r, u, "กรุณาเลือกอาหารอย่างน้อย 1 รายการ", http.StatusBadRequest)
		return
	}
	var total float64
	for id, q := range items {
		if q > 50 {
			a.renderOrderForm(w, r, u, "จำนวนต่อรายการต้องไม่เกิน 50", http.StatusBadRequest)
			return
		}
		total += prices[id] * float64(q) // ราคาจากฐานข้อมูลเท่านั้น ไม่เชื่อค่าจากฟอร์ม
	}

	orderType := "normal"
	var resID sql.NullInt64
	if r.PostFormValue("order_type") == "reservation" {
		id, _ := strconv.ParseInt(r.PostFormValue("reservation_id"), 10, 64)
		res, err := a.getRes(id)
		if err != nil || res.UserID != u.ID { // ผูกได้เฉพาะการจองของตัวเอง
			a.renderOrderForm(w, r, u, "กรุณาเลือกการจองของคุณ", http.StatusBadRequest)
			return
		}
		orderType = "reservation"
		resID = sql.NullInt64{Int64: id, Valid: true}
	}
	note := strings.TrimSpace(r.PostFormValue("note"))
	if len([]rune(note)) > 200 {
		a.renderOrderForm(w, r, u, "หมายเหตุยาวเกิน 200 ตัวอักษร", http.StatusBadRequest)
		return
	}

	var orderID int64
	if err := a.db.QueryRow(`INSERT INTO orders(user_id,total_price,order_type,reservation_id,note) VALUES(?,?,?,?,?) RETURNING id`,
		u.ID, total, orderType, resID, note).Scan(&orderID); err != nil {
		a.serverErr(w, err)
		return
	}
	for id, q := range items {
		if _, err := a.db.Exec(`INSERT INTO order_items(order_id,menu_item_id,quantity,price) VALUES(?,?,?,?)`,
			orderID, id, q, prices[id]); err != nil {
			a.db.Exec(`DELETE FROM orders WHERE id=?`, orderID)
			a.serverErr(w, err)
			return
		}
	}
	http.Redirect(w, r, "/my-orders?ok=1", http.StatusSeeOther)
}

func (a *App) listOrders(userID int64) []OrderView {
	q := `SELECT o.id,u.username,o.order_type,COALESCE(r.when_at,''),o.total_price,o.status,o.created_at,o.note
		FROM orders o JOIN users u ON u.id=o.user_id LEFT JOIN reservations r ON r.id=o.reservation_id`
	args := []any{}
	if userID != 0 {
		q += ` WHERE o.user_id=?`
		args = append(args, userID)
	}
	rows, err := a.db.Query(q+` ORDER BY o.id DESC LIMIT 50`, args...)
	if err != nil {
		log.Println("listOrders:", err)
		return nil
	}
	var out []OrderView
	for rows.Next() {
		var o OrderView
		rows.Scan(&o.ID, &o.Username, &o.Type, &o.ResWhen, &o.Total, &o.Status, &o.CreatedAt, &o.Note)
		out = append(out, o)
	}
	rows.Close()
	for i := range out {
		ir, err := a.db.Query(`SELECT m.name,i.quantity,i.price FROM order_items i JOIN menu_items m ON m.id=i.menu_item_id WHERE i.order_id=?`, out[i].ID)
		if err != nil {
			continue
		}
		for ir.Next() {
			var it OrderItemView
			ir.Scan(&it.Name, &it.Qty, &it.Price)
			out[i].Items = append(out[i].Items, it)
		}
		ir.Close()
	}
	return out
}

func (a *App) orderHistory(w http.ResponseWriter, r *http.Request, u *User) {
	p := a.page(w, r)
	if u.Role == "admin" {
		p.Data = a.listOrders(0)
	} else {
		p.Data = a.listOrders(u.ID)
	}
	a.render(w, "orders", p, http.StatusOK)
}

// ลูกค้ายกเลิกได้เฉพาะออเดอร์ของตัวเองที่ยังรอดำเนินการ
func (a *App) orderCancel(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	res, err := a.db.Exec(`UPDATE orders SET status='cancelled' WHERE id=? AND user_id=? AND status='pending'`, id, u.ID)
	if err != nil {
		a.serverErr(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, "ยกเลิกไม่ได้ (ไม่ใช่ออเดอร์ของคุณ หรือไม่ได้อยู่ในสถานะรอดำเนินการ)", http.StatusForbidden)
		return
	}
	http.Redirect(w, r, "/my-orders?ok=1", http.StatusSeeOther)
}

// แอดมินเปลี่ยนสถานะออเดอร์
func (a *App) orderStatus(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	st := r.PostFormValue("status")
	switch st {
	case "pending", "preparing", "done", "cancelled":
	default:
		http.Error(w, "สถานะไม่ถูกต้อง", http.StatusBadRequest)
		return
	}
	a.db.Exec(`UPDATE orders SET status=? WHERE id=?`, st, id)
	http.Redirect(w, r, "/my-orders?ok=1", http.StatusSeeOther)
}

// ---------- สมัคร / เข้าสู่ระบบ ----------

func (a *App) registerForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, "register", a.page(w, r), 200)
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	p := a.page(w, r)
	username := strings.TrimSpace(r.PostFormValue("username"))
	email := strings.TrimSpace(r.PostFormValue("email"))
	pw := r.PostFormValue("password")
	switch {
	case !usernameRe.MatchString(username):
		p.Error = "ชื่อผู้ใช้ต้องเป็น a-z, 0-9, _ ยาว 3-30 ตัว"
	case !strings.Contains(email, "@") || len(email) > 100:
		p.Error = "อีเมลไม่ถูกต้อง"
	case len(pw) < 8 || len(pw) > 72:
		p.Error = "รหัสผ่านต้องยาว 8-72 ตัวอักษร"
	}
	if p.Error != "" {
		a.render(w, "register", p, 400)
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		a.serverErr(w, err)
		return
	}
	if _, err := a.db.Exec(`INSERT INTO users(username,email,password_hash) VALUES(?,?,?)`, username, email, string(h)); err != nil {
		p.Error = "ชื่อผู้ใช้นี้ถูกใช้แล้ว"
		a.render(w, "register", p, 409)
		return
	}
	http.Redirect(w, r, "/login?ok=1", http.StatusSeeOther)
}

func (a *App) loginForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, "login", a.page(w, r), 200)
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	p := a.page(w, r)
	ip := clientIP(r)
	if a.tooMany(ip) {
		p.Error = "ลองผิดหลายครั้งเกินไป กรุณารอ 10 นาที"
		a.render(w, "login", p, 429)
		return
	}
	var id int64
	var hash string
	err := a.db.QueryRow(`SELECT id,password_hash FROM users WHERE username=?`, strings.TrimSpace(r.PostFormValue("username"))).Scan(&id, &hash)
	if err != nil {
		hash = string(dummyHash) // กัน timing attack
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.PostFormValue("password"))) != nil || err != nil {
		a.addFail(ip)
		p.Error = "ชื่อผู้ใช้หรือรหัสผ่านไม่ถูกต้อง"
		a.render(w, "login", p, 401)
		return
	}
	t := token()
	exp := time.Now().Add(24 * time.Hour)
	if _, err := a.db.Exec(`INSERT INTO sessions(token,user_id,expires_at) VALUES(?,?,?)`, t, id, exp.Unix()); err != nil {
		a.serverErr(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: t, Path: "/", Expires: exp, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: secureCookie()})
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sid"); err == nil {
		a.db.Exec(`DELETE FROM sessions WHERE token=?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- แดชบอร์ด ----------

func (a *App) listRes(userID int64) []Reservation {
	q := `SELECT r.id,r.user_id,u.username,r.name,r.phone,r.party_size,r.when_at,r.note
		FROM reservations r JOIN users u ON u.id=r.user_id`
	args := []any{}
	if userID != 0 {
		q += ` WHERE r.user_id=?`
		args = append(args, userID)
	}
	rows, err := a.db.Query(q+` ORDER BY r.when_at`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Reservation
	for rows.Next() {
		var x Reservation
		rows.Scan(&x.ID, &x.UserID, &x.Username, &x.Name, &x.Phone, &x.PartySize, &x.When, &x.Note)
		out = append(out, x)
	}
	return out
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request, u *User) {
	p := a.page(w, r)
	d := struct {
		Reservations            []Reservation
		Users, Resv, Revs, Menu int
	}{}
	if u.Role == "admin" {
		d.Reservations = a.listRes(0)
		a.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&d.Users)
		a.db.QueryRow(`SELECT COUNT(*) FROM reservations`).Scan(&d.Resv)
		a.db.QueryRow(`SELECT COUNT(*) FROM reviews`).Scan(&d.Revs)
		a.db.QueryRow(`SELECT COUNT(*) FROM menu_items`).Scan(&d.Menu)
	} else {
		d.Reservations = a.listRes(u.ID)
	}
	p.Data = d
	a.render(w, "dashboard", p, 200)
}

// ---------- CRUD การจองโต๊ะ (มีเจ้าของข้อมูล) ----------

type resForm struct {
	Action string
	R      Reservation
	Edit   bool
}

func (a *App) getRes(id int64) (Reservation, error) {
	var x Reservation
	err := a.db.QueryRow(`SELECT id,user_id,name,phone,party_size,when_at,note FROM reservations WHERE id=?`, id).
		Scan(&x.ID, &x.UserID, &x.Name, &x.Phone, &x.PartySize, &x.When, &x.Note)
	return x, err
}

func parseRes(r *http.Request) (Reservation, string) {
	x := Reservation{
		Name:  strings.TrimSpace(r.PostFormValue("name")),
		Phone: strings.TrimSpace(r.PostFormValue("phone")),
		Note:  strings.TrimSpace(r.PostFormValue("note")),
		When:  strings.Replace(strings.TrimSpace(r.PostFormValue("when")), "T", " ", 1),
	}
	x.PartySize, _ = strconv.Atoi(r.PostFormValue("party_size"))
	t, err := time.ParseInLocation("2006-01-02 15:04", x.When, ict)
	switch {
	case x.Name == "" || len(x.Name) > 80:
		return x, "กรุณากรอกชื่อ (ไม่เกิน 80 ตัวอักษร)"
	case !phoneRe.MatchString(x.Phone):
		return x, "เบอร์โทรไม่ถูกต้อง"
	case x.PartySize < 1 || x.PartySize > 20:
		return x, "จำนวนคนต้อง 1-20"
	case err != nil || t.Before(time.Now()):
		return x, "กรุณาเลือกวัน-เวลาในอนาคต"
	case len(x.Note) > 300:
		return x, "หมายเหตุยาวเกิน 300 ตัวอักษร"
	}
	return x, ""
}

func (a *App) resNew(w http.ResponseWriter, r *http.Request, u *User) {
	p := a.page(w, r)
	p.Data = resForm{Action: "/reservations", R: Reservation{PartySize: 2}}
	a.render(w, "reservation_form", p, 200)
}

func (a *App) resCreate(w http.ResponseWriter, r *http.Request, u *User) {
	x, msg := parseRes(r)
	if msg != "" {
		p := a.page(w, r)
		p.Error, p.Data = msg, resForm{Action: "/reservations", R: x}
		a.render(w, "reservation_form", p, 400)
		return
	}
	var newID int64
	if err := a.db.QueryRow(`INSERT INTO reservations(user_id,name,phone,party_size,when_at,note) VALUES(?,?,?,?,?,?) RETURNING id`,
		u.ID, x.Name, x.Phone, x.PartySize, x.When, x.Note).Scan(&newID); err != nil {
		a.serverErr(w, err)
		return
	}
	http.Redirect(w, r, "/order?reservation="+strconv.FormatInt(newID, 10)+"&new=1", http.StatusSeeOther)
}

func (a *App) ownedRes(w http.ResponseWriter, r *http.Request, u *User, adminOK bool) (Reservation, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	x, err := a.getRes(id)
	if err != nil {
		http.NotFound(w, r)
		return x, false
	}
	if x.UserID != u.ID && !(adminOK && u.Role == "admin") {
		http.Error(w, "ไม่มีสิทธิ์จัดการข้อมูลนี้", http.StatusForbidden)
		return x, false
	}
	return x, true
}

func (a *App) resEdit(w http.ResponseWriter, r *http.Request, u *User) {
	x, ok := a.ownedRes(w, r, u, false)
	if !ok {
		return
	}
	p := a.page(w, r)
	p.Data = resForm{Action: "/reservations/" + strconv.FormatInt(x.ID, 10) + "/edit", R: x, Edit: true}
	a.render(w, "reservation_form", p, 200)
}

func (a *App) resUpdate(w http.ResponseWriter, r *http.Request, u *User) {
	old, ok := a.ownedRes(w, r, u, false)
	if !ok {
		return
	}
	x, msg := parseRes(r)
	if msg != "" {
		p := a.page(w, r)
		x.ID = old.ID
		p.Error, p.Data = msg, resForm{Action: "/reservations/" + strconv.FormatInt(old.ID, 10) + "/edit", R: x, Edit: true}
		a.render(w, "reservation_form", p, 400)
		return
	}
	a.db.Exec(`UPDATE reservations SET name=?,phone=?,party_size=?,when_at=?,note=? WHERE id=? AND user_id=?`,
		x.Name, x.Phone, x.PartySize, x.When, x.Note, old.ID, old.UserID)
	http.Redirect(w, r, "/dashboard?ok=1", http.StatusSeeOther)
}

func (a *App) resDelete(w http.ResponseWriter, r *http.Request, u *User) {
	x, ok := a.ownedRes(w, r, u, true)
	if !ok {
		return
	}
	a.db.Exec(`DELETE FROM reservations WHERE id=?`, x.ID)
	http.Redirect(w, r, "/dashboard?ok=1", http.StatusSeeOther)
}

// ---------- รีวิว ----------

func (a *App) reviews(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT v.id,v.user_id,u.username,v.rating,v.comment,v.created_at
		FROM reviews v JOIN users u ON u.id=v.user_id ORDER BY v.id DESC LIMIT 100`)
	if err != nil {
		a.serverErr(w, err)
		return
	}
	defer rows.Close()
	var list []Review
	for rows.Next() {
		var v Review
		rows.Scan(&v.ID, &v.UserID, &v.Username, &v.Rating, &v.Comment, &v.CreatedAt)
		list = append(list, v)
	}
	p := a.page(w, r)
	p.Data = list
	a.render(w, "reviews", p, 200)
}

func (a *App) reviewCreate(w http.ResponseWriter, r *http.Request, u *User) {
	rating, _ := strconv.Atoi(r.PostFormValue("rating"))
	comment := strings.TrimSpace(r.PostFormValue("comment"))
	if rating < 1 || rating > 5 || comment == "" || len([]rune(comment)) > 500 {
		http.Error(w, "ข้อมูลรีวิวไม่ถูกต้อง", http.StatusBadRequest)
		return
	}
	a.db.Exec(`INSERT INTO reviews(user_id,rating,comment) VALUES(?,?,?)`, u.ID, rating, comment)
	http.Redirect(w, r, "/reviews?ok=1", http.StatusSeeOther)
}

func (a *App) reviewDelete(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var owner int64
	if a.db.QueryRow(`SELECT user_id FROM reviews WHERE id=?`, id).Scan(&owner) != nil {
		http.NotFound(w, r)
		return
	}
	if owner != u.ID && u.Role != "admin" {
		http.Error(w, "ไม่มีสิทธิ์ลบรีวิวนี้", http.StatusForbidden)
		return
	}
	a.db.Exec(`DELETE FROM reviews WHERE id=?`, id)
	http.Redirect(w, r, "/reviews?ok=1", http.StatusSeeOther)
}

// ---------- แอดมิน: จัดการเมนู ----------

func (a *App) menuList(w http.ResponseWriter, r *http.Request, u *User) {
	p := a.page(w, r)
	p.Data = a.menuItems()
	a.render(w, "admin_menu", p, 200)
}

func (a *App) menuCreate(w http.ResponseWriter, r *http.Request, u *User) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	cat := strings.TrimSpace(r.PostFormValue("category"))
	desc := strings.TrimSpace(r.PostFormValue("description"))
	price, err := strconv.ParseFloat(r.PostFormValue("price"), 64)
	if name == "" || cat == "" || len(name) > 100 || len(desc) > 300 || err != nil || price < 0 || price > 100000 {
		http.Error(w, "ข้อมูลเมนูไม่ถูกต้อง", http.StatusBadRequest)
		return
	}
	img, ok := cleanImageURL(r.PostFormValue("image_url"))
	if !ok {
		http.Error(w, "ลิงก์รูปต้องขึ้นต้นด้วย /static/ หรือ https://", http.StatusBadRequest)
		return
	}
	a.db.Exec(`INSERT INTO menu_items(name,description,price,category,image_url) VALUES(?,?,?,?,?)`, name, desc, price, cat, img)
	http.Redirect(w, r, "/admin/menu?ok=1", http.StatusSeeOther)
}

// รับเฉพาะรูปในโฟลเดอร์ /static/ หรือลิงก์ https:// (กัน javascript: และลิงก์แปลกๆ)
func cleanImageURL(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", true
	}
	if len(v) > 300 || strings.Contains(v, "..") || strings.ContainsAny(v, "\"'<>\n\r") {
		return "", false
	}
	if strings.HasPrefix(v, "/static/") || strings.HasPrefix(v, "https://") {
		return v, true
	}
	return "", false
}

func (a *App) menuImage(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	img, ok := cleanImageURL(r.PostFormValue("image_url"))
	if !ok {
		http.Error(w, "ลิงก์รูปต้องขึ้นต้นด้วย /static/ หรือ https://", http.StatusBadRequest)
		return
	}
	a.db.Exec(`UPDATE menu_items SET image_url=? WHERE id=?`, img, id)
	http.Redirect(w, r, "/admin/menu?ok=1", http.StatusSeeOther)
}

func (a *App) menuDelete(w http.ResponseWriter, r *http.Request, u *User) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, err := a.db.Exec(`DELETE FROM menu_items WHERE id=?`, id); err != nil {
		http.Error(w, "ลบไม่ได้ เพราะเมนูนี้อยู่ในประวัติการสั่งอาหาร", http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/admin/menu?ok=1", http.StatusSeeOther)
}
