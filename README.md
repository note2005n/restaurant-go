# ครัวบ้านเรา – ระบบจองโต๊ะและรีวิวร้านอาหาร (Go)

## รันบนเครื่อง
    go mod tidy
    ADMIN_USER=admin ADMIN_PASS='รหัสผ่านของคุณ' go run .
เปิด http://localhost:8080

## Deploy (Render / Railway / Fly.io)
ใช้ Dockerfile ที่แนบมา ตั้งค่า env: ADMIN_USER, ADMIN_PASS, COOKIE_SECURE=1
และผูก persistent disk ที่ /data เพื่อเก็บไฟล์ SQLite

## ตรงตามเกณฑ์
1. สมัคร/ล็อกอิน + bcrypt password hashing
2. 2 บทบาท: customer / admin
3. CRUD "การจองโต๊ะ" มีเจ้าของ (ตรวจ user_id ก่อนแก้/ลบ, แอดมินลบได้)
4. ฟอร์มบันทึกลง DB: สมัครสมาชิก, จองโต๊ะ, รีวิว, เพิ่มเมนู (แอดมิน)
5. หน้าผู้ดูแล: /dashboard (สถิติ) และ /admin/menu

## การป้องกันการโจมตี
SQL injection (prepared statements), XSS (html/template + CSP), CSRF (double-submit token + SameSite),
brute-force (จำกัดล็อกอิน 5 ครั้ง/10 นาที), session สุ่ม 256 บิต HttpOnly+Secure, หมดอายุ 24 ชม.,
IDOR (ตรวจเจ้าของ), จำกัดขนาด body, security headers, validation ฝั่งเซิร์ฟเวอร์, กัน timing attack ตอนล็อกอิน

## ฐานข้อมูล
- ไม่มี `DATABASE_URL` → ใช้ SQLite (ไฟล์ restaurant.db) เหมาะกับรันในเครื่อง
- มี `DATABASE_URL` (postgres://...) → ใช้ PostgreSQL อัตโนมัติ ข้อมูลไม่หายเมื่อรีสตาร์ท
