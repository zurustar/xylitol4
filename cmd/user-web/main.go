package main

import (
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"xylitol4/sip/userdb"
)

type server struct {
	store        *userdb.SQLiteStore
	adminUser    string
	adminPass    string
	adminTmpl    *template.Template
	passwordTmpl *template.Template
	homeTmpl     *template.Template
}

func main() {
	listen := flag.String("listen", ":8080", "HTTP address to listen on (host:port)")
	userDBPath := flag.String("user-db", "", "Path to the SQLite user database")
	adminUser := flag.String("admin-user", "", "Username required for admin endpoints")
	adminPass := flag.String("admin-pass", "", "Password required for admin endpoints")
	flag.Parse()

	if strings.TrimSpace(*userDBPath) == "" {
		log.Fatal("--user-db is required")
	}
	if strings.TrimSpace(*adminUser) == "" || strings.TrimSpace(*adminPass) == "" {
		log.Fatal("--admin-user and --admin-pass are required")
	}

	store, err := userdb.OpenSQLite(*userDBPath)
	if err != nil {
		log.Fatalf("failed to open user database: %v", err)
	}
	defer store.Close()

	srv := &server{
		store:        store,
		adminUser:    *adminUser,
		adminPass:    *adminPass,
		adminTmpl:    template.Must(template.New("admin").Parse(adminTemplate)),
		passwordTmpl: template.Must(template.New("password").Parse(passwordTemplate)),
		homeTmpl:     template.Must(template.New("home").Parse(homeTemplate)),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleHome)
	mux.HandleFunc("/admin/users", srv.basicAuth(srv.handleAdminUsers))
	mux.HandleFunc("/password", srv.handlePassword)

	server := &http.Server{
		Addr:         *listen,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	log.Printf("user web interface listening on %s", *listen)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server failed: %v", err)
	}
}

func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.homeTmpl.Execute(w, nil); err != nil {
		log.Printf("render home: %v", err)
	}
}

func (s *server) basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !s.authorisedAdmin(user, pass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="admin"`)
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *server) authorisedAdmin(user, pass string) bool {
	return subtleCompare(user, s.adminUser) && subtleCompare(pass, s.adminPass)
}

func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

type adminTemplateData struct {
	Users   []userdb.User
	Message string
	Error   string
}

func (s *server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := adminTemplateData{}

	switch r.Method {
	case http.MethodGet:
		// no-op, fall through to listing
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			data.Error = fmt.Sprintf("フォームの解析に失敗しました: %v", err)
			break
		}
		action := r.FormValue("action")
		username := strings.TrimSpace(r.FormValue("username"))
		domain := strings.TrimSpace(r.FormValue("domain"))
		contact := strings.TrimSpace(r.FormValue("contact"))
		if username == "" || domain == "" {
			data.Error = "ユーザ名とドメインを入力してください"
			break
		}
		switch action {
		case "create":
			password := r.FormValue("password")
			var hash string
			if password != "" {
				hash = userdb.HashPassword(username, domain, password)
			}
			err := s.store.CreateUser(ctx, userdb.User{
				Username:     username,
				Domain:       domain,
				PasswordHash: hash,
				ContactURI:   contact,
			})
			if err != nil {
				data.Error = fmt.Sprintf("ユーザ作成に失敗しました: %v", err)
			} else {
				data.Message = fmt.Sprintf("ユーザ %s@%s を登録しました", username, domain)
			}
		case "delete":
			if err := s.store.DeleteUser(ctx, username, domain); err != nil {
				data.Error = fmt.Sprintf("ユーザ削除に失敗しました: %v", err)
			} else {
				data.Message = fmt.Sprintf("ユーザ %s@%s を削除しました", username, domain)
			}
		default:
			data.Error = "不明な操作が指定されました"
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	users, err := s.store.AllUsers(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list users: %v", err), http.StatusInternalServerError)
		return
	}
	sort.Slice(users, func(i, j int) bool {
		if users[i].Domain == users[j].Domain {
			return users[i].Username < users[j].Username
		}
		return users[i].Domain < users[j].Domain
	})
	data.Users = users

	if err := s.adminTmpl.Execute(w, data); err != nil {
		log.Printf("render admin: %v", err)
	}
}

type passwordTemplateData struct {
	Message string
	Error   string
}

func (s *server) handlePassword(w http.ResponseWriter, r *http.Request) {
	data := passwordTemplateData{}
	switch r.Method {
	case http.MethodGet:
		// nothing to do
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			data.Error = fmt.Sprintf("フォームの解析に失敗しました: %v", err)
			break
		}
		username := strings.TrimSpace(r.FormValue("username"))
		domain := strings.TrimSpace(r.FormValue("domain"))
		current := r.FormValue("current_password")
		newPassword := r.FormValue("new_password")
		confirm := r.FormValue("confirm_password")

		if username == "" || domain == "" {
			data.Error = "ユーザ名とドメインを入力してください"
			break
		}
		if newPassword == "" {
			data.Error = "新しいパスワードを入力してください"
			break
		}
		if newPassword != confirm {
			data.Error = "新しいパスワードが確認と一致しません"
			break
		}

		ctx := r.Context()
		user, err := s.store.Lookup(ctx, username, domain)
		if err != nil {
			data.Error = fmt.Sprintf("ユーザ情報の取得に失敗しました: %v", err)
			break
		}

		if user.PasswordHash != "" && !userdb.VerifyPassword(user.PasswordHash, username, domain, current) {
			data.Error = "現在のパスワードが正しくありません"
			break
		}

		hash := userdb.HashPassword(username, domain, newPassword)
		if err := s.store.UpdatePassword(ctx, username, domain, hash); err != nil {
			data.Error = fmt.Sprintf("パスワードの更新に失敗しました: %v", err)
			break
		}
		data.Message = "パスワードを更新しました"
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.passwordTmpl.Execute(w, data); err != nil {
		log.Printf("render password: %v", err)
	}
}

const homeTemplate = `<!DOCTYPE html>
<html lang="ja">
<head>
        <meta charset="UTF-8">
        <title>ユーザ管理</title>
        <style>
                body { font-family: sans-serif; margin: 2rem; }
                a { display: block; margin-bottom: 1rem; }
        </style>
</head>
<body>
        <h1>ユーザ管理ポータル</h1>
        <a href="/admin/users">管理者: ユーザ一覧/登録/削除</a>
        <a href="/password">利用者: パスワード変更</a>
</body>
</html>`

const adminTemplate = `<!DOCTYPE html>
<html lang="ja">
<head>
        <meta charset="UTF-8">
        <title>管理者 - ユーザ管理</title>
        <style>
                body { font-family: sans-serif; margin: 2rem; }
                table { border-collapse: collapse; margin-top: 1rem; width: 100%; max-width: 800px; }
                th, td { border: 1px solid #ccc; padding: 0.5rem; text-align: left; }
                form { margin-top: 1rem; }
                .message { color: green; }
                .error { color: red; }
        </style>
</head>
<body>
        <h1>管理者 - ユーザ管理</h1>
        {{if .Message}}<p class="message">{{.Message}}</p>{{end}}
        {{if .Error}}<p class="error">{{.Error}}</p>{{end}}

        <h2>登録ユーザ一覧</h2>
        <table>
                <thead>
                        <tr><th>ユーザ名</th><th>ドメイン</th><th>Contact URI</th></tr>
                </thead>
                <tbody>
                        {{range .Users}}
                        <tr>
                                <td>{{.Username}}</td>
                                <td>{{.Domain}}</td>
                                <td>{{.ContactURI}}</td>
                        </tr>
                        {{else}}
                        <tr><td colspan="3">登録されたユーザはいません</td></tr>
                        {{end}}
                </tbody>
        </table>

        <h2>新規ユーザ登録</h2>
        <form method="post">
                <input type="hidden" name="action" value="create">
                <label>ユーザ名: <input type="text" name="username" required></label><br>
                <label>ドメイン: <input type="text" name="domain" required></label><br>
                <label>初期パスワード (任意): <input type="password" name="password"></label><br>
                <label>Contact URI (任意): <input type="text" name="contact"></label><br>
                <button type="submit">登録</button>
        </form>

        <h2>ユーザ削除</h2>
        <form method="post">
                <input type="hidden" name="action" value="delete">
                <label>ユーザ名: <input type="text" name="username" required></label><br>
                <label>ドメイン: <input type="text" name="domain" required></label><br>
                <button type="submit">削除</button>
        </form>

        <p><a href="/">ポータルトップへ戻る</a></p>
</body>
</html>`

const passwordTemplate = `<!DOCTYPE html>
<html lang="ja">
<head>
        <meta charset="UTF-8">
        <title>パスワード変更</title>
        <style>
                body { font-family: sans-serif; margin: 2rem; }
                form { max-width: 400px; }
                label { display: block; margin-top: 1rem; }
                .message { color: green; }
                .error { color: red; }
        </style>
</head>
<body>
        <h1>パスワード変更</h1>
        {{if .Message}}<p class="message">{{.Message}}</p>{{end}}
        {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
        <form method="post">
                <label>ユーザ名: <input type="text" name="username" required></label>
                <label>ドメイン: <input type="text" name="domain" required></label>
                <label>現在のパスワード: <input type="password" name="current_password"></label>
                <label>新しいパスワード: <input type="password" name="new_password" required></label>
                <label>新しいパスワード(確認): <input type="password" name="confirm_password" required></label>
                <button type="submit">更新</button>
        </form>
        <p><a href="/">ポータルトップへ戻る</a></p>
</body>
</html>`
