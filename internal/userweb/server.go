package userweb

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"xylitol4/sip/userdb"
)

// Config captures the dependencies required to expose the user management web UI.
type Config struct {
	Store     *userdb.SQLiteStore
	AdminUser string
	AdminPass string
	Logger    *log.Logger
}

// Server serves the combined administrative and self-service web interface.
type Server struct {
	store        *userdb.SQLiteStore
	adminUser    string
	adminPass    string
	adminTmpl    *template.Template
	passwordTmpl *template.Template
	homeTmpl     *template.Template
	logger       *log.Logger
}

// New constructs a Server using the provided configuration.
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("userweb: store is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}

	adminTmpl, err := template.New("admin").Parse(adminTemplate)
	if err != nil {
		return nil, fmt.Errorf("userweb: parse admin template: %w", err)
	}
	passwordTmpl, err := template.New("password").Parse(passwordTemplate)
	if err != nil {
		return nil, fmt.Errorf("userweb: parse password template: %w", err)
	}
	homeTmpl, err := template.New("home").Parse(homeTemplate)
	if err != nil {
		return nil, fmt.Errorf("userweb: parse home template: %w", err)
	}

	return &Server{
		store:        cfg.Store,
		adminUser:    cfg.AdminUser,
		adminPass:    cfg.AdminPass,
		adminTmpl:    adminTmpl,
		passwordTmpl: passwordTmpl,
		homeTmpl:     homeTmpl,
		logger:       logger,
	}, nil
}

// Handler returns an http.Handler wiring the user web routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHome)
	mux.HandleFunc("/admin/users", s.basicAuth(s.handleAdminUsers))
	mux.HandleFunc("/password", s.handlePassword)
	return mux
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.homeTmpl.Execute(w, nil); err != nil {
		s.logger.Printf("render home: %v", err)
	}
}

func (s *Server) basicAuth(next http.HandlerFunc) http.HandlerFunc {
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

func (s *Server) authorisedAdmin(user, pass string) bool {
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
	Users          []userdb.User
	BroadcastRules []userdb.BroadcastRule
	Message        string
	Error          string
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
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
		switch action {
		case "create":
			username := strings.TrimSpace(r.FormValue("username"))
			domain := strings.TrimSpace(r.FormValue("domain"))
			contact := strings.TrimSpace(r.FormValue("contact"))
			if username == "" || domain == "" {
				data.Error = "ユーザ名とドメインを入力してください"
				break
			}
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
				data.Message = fmt.Sprintf("ユーザ %s@%s を登録ました", username, domain)
			}
		case "delete":
			username := strings.TrimSpace(r.FormValue("username"))
			domain := strings.TrimSpace(r.FormValue("domain"))
			if username == "" || domain == "" {
				data.Error = "ユーザ名とドメインを入力してください"
				break
			}
			if err := s.store.DeleteUser(ctx, username, domain); err != nil {
				data.Error = fmt.Sprintf("ユーザ削除に失敗しました: %v", err)
			} else {
				data.Message = fmt.Sprintf("ユーザ %s@%s を削除しました", username, domain)
			}
		case "broadcast-create":
			address := strings.TrimSpace(r.FormValue("broadcast_address"))
			description := strings.TrimSpace(r.FormValue("broadcast_description"))
			targets := parseBroadcastTargets(r.FormValue("broadcast_targets"))
			if address == "" {
				data.Error = "ブロードキャスト対象アドレスを入力してください"
				break
			}
			_, err := s.store.CreateBroadcastRule(ctx, userdb.BroadcastRule{
				Address:     address,
				Description: description,
				Targets:     targets,
			})
			if err != nil {
				data.Error = fmt.Sprintf("ブロードキャストルールの作成に失敗しました: %v", err)
			} else {
				data.Message = fmt.Sprintf("%s のブロードキャストルールを作成しました", address)
			}
		case "broadcast-update":
			idStr := strings.TrimSpace(r.FormValue("broadcast_id"))
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil || id <= 0 {
				data.Error = "更新対象のルールIDが正しくありません"
				break
			}
			address := strings.TrimSpace(r.FormValue("broadcast_address"))
			description := strings.TrimSpace(r.FormValue("broadcast_description"))
			targets := parseBroadcastTargets(r.FormValue("broadcast_targets"))
			if address == "" {
				data.Error = "ブロードキャスト対象アドレスを入力してください"
				break
			}
			update := userdb.BroadcastRule{ID: id, Address: address, Description: description}
			if err := s.store.UpdateBroadcastRule(ctx, update); err != nil {
				data.Error = fmt.Sprintf("ブロードキャストルールの更新に失敗しました: %v", err)
				break
			}
			if err := s.store.ReplaceBroadcastTargets(ctx, id, targets); err != nil {
				data.Error = fmt.Sprintf("宛先URIの更新に失敗しました: %v", err)
				break
			}
			data.Message = fmt.Sprintf("ルールID %d を更新しました", id)
		case "broadcast-delete":
			idStr := strings.TrimSpace(r.FormValue("broadcast_id"))
			id, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil || id <= 0 {
				data.Error = "削除対象のルールIDが正しくありません"
				break
			}
			if err := s.store.DeleteBroadcastRule(ctx, id); err != nil {
				data.Error = fmt.Sprintf("ブロードキャストルールの削除に失敗しました: %v", err)
			} else {
				data.Message = fmt.Sprintf("ルールID %d を削除しました", id)
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

	rules, err := s.store.ListBroadcastRules(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to list broadcast rules: %v", err), http.StatusInternalServerError)
		return
	}
	data.BroadcastRules = rules

	if err := s.adminTmpl.Execute(w, data); err != nil {
		s.logger.Printf("render admin: %v", err)
	}
}

type passwordTemplateData struct {
	Message string
	Error   string
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
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
		s.logger.Printf("render password: %v", err)
	}
}

func parseBroadcastTargets(raw string) []userdb.BroadcastTarget {
	var targets []userdb.BroadcastTarget
	if strings.TrimSpace(raw) == "" {
		return targets
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case '\n', '\r', ',', ';':
			return true
		default:
			return false
		}
	})
	order := 0
	for _, part := range parts {
		contact := strings.TrimSpace(part)
		if contact == "" {
			continue
		}
		targets = append(targets, userdb.BroadcastTarget{ContactURI: contact, Priority: order})
		order++
	}
	return targets
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

        <h2>ブロードキャストルール</h2>
        <table>
                <thead>
                        <tr><th>ID</th><th>Address</th><th>Description</th><th>Targets</th></tr>
                </thead>
                <tbody>
                        {{range .BroadcastRules}}
                        <tr>
                                <td>{{.ID}}</td>
                                <td>{{.Address}}</td>
                                <td>{{.Description}}</td>
                                <td>
                                        {{range .Targets}}
                                        <div>{{.ContactURI}}</div>
                                        {{else}}
                                        <div>(なし)</div>
                                        {{end}}
                                </td>
                        </tr>
                        {{else}}
                        <tr><td colspan="4">登録されたルールはありません</td></tr>
                        {{end}}
                </tbody>
        </table>

        <h2>ブロードキャストルール作成</h2>
        <form method="post">
                <input type="hidden" name="action" value="broadcast-create">
                <label>Address: <input type="text" name="broadcast_address" required></label><br>
                <label>Description: <input type="text" name="broadcast_description"></label><br>
                <label>Targets (改行・カンマ区切り):<br><textarea name="broadcast_targets" rows="4" cols="40"></textarea></label><br>
                <button type="submit">作成</button>
        </form>

        <h2>ブロードキャストルール更新</h2>
        <form method="post">
                <input type="hidden" name="action" value="broadcast-update">
                <label>ID: <input type="number" name="broadcast_id" min="1" required></label><br>
                <label>Address: <input type="text" name="broadcast_address" required></label><br>
                <label>Description: <input type="text" name="broadcast_description"></label><br>
                <label>Targets (改行・カンマ区切り):<br><textarea name="broadcast_targets" rows="4" cols="40"></textarea></label><br>
                <button type="submit">更新</button>
        </form>

        <h2>ブロードキャストルール削除</h2>
        <form method="post">
                <input type="hidden" name="action" value="broadcast-delete">
                <label>ID: <input type="number" name="broadcast_id" min="1" required></label><br>
                <button type="submit">削除</button>
        </form>
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
                label { display: block; margin-bottom: 0.5rem; }
                input { width: 100%; padding: 0.4rem; margin-top: 0.2rem; }
                .message { color: green; }
                .error { color: red; }
        </style>
</head>
<body>
        <h1>パスワード変更</h1>
        {{if .Message}}<p class="message">{{.Message}}</p>{{end}}
        {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
        <form method="post">
                <label>ユーザ名<input type="text" name="username" required></label>
                <label>ドメイン<input type="text" name="domain" required></label>
                <label>現在のパスワード<input type="password" name="current_password"></label>
                <label>新しいパスワード<input type="password" name="new_password" required></label>
                <label>新しいパスワード(確認)<input type="password" name="confirm_password" required></label>
                <button type="submit">変更</button>
        </form>
        <a href="/">戻る</a>
</body>
</html>`
