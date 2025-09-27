# xylitol4

このリポジトリには RFC 3261 に準拠したステートフルな SIP プロキシ実装が含まれます。`cmd/sip-proxy`
に配置されたエントリーポイントをビルドすることで、UDP ベースのシンプルなプロキシソフトウェアとして
起動できます。

## ビルドと実行

```bash
go build -o sip-proxy ./cmd/sip-proxy
./sip-proxy --listen :5060 --upstream 192.0.2.10:5060 --user-db ./users.db
```

開発時にはバイナリを生成せずに `go run ./cmd/sip-proxy --upstream 192.0.2.10:5060` のように直接起動する
ことも可能です。

主なオプションは以下のとおりです。

- `--listen`: 下流クライアントからのパケットを受け付ける UDP アドレス (デフォルト `:5060`)
- `--upstream`: 上流の SIP サーバーに転送する UDP アドレス。省略した場合は、登録済みクライアントまたは Request-URI の名前解決に基づいて転送先を決定します。
- `--upstream-bind`: 上流サーバーと通信する際に使用するローカル UDP アドレス (省略時は OS が割り当て)
- `--route-ttl`: クライアントのトランザクションルートを保持する時間 (デフォルト 5 分)
- `--user-db`: SIP ユーザ情報が格納された SQLite データベースファイルのパス (必須)

プロセスは `SIGINT` または `SIGTERM` を受け取ると安全にシャットダウンします。

## ユーザ管理Webインタフェース

ユーザアカウントの登録やパスワード変更を行うには、`cmd/user-web`に含まれるHTTPサーバを起動する。

```bash
go run ./cmd/user-web --listen :8080 --user-db ./users.db --admin-user admin --admin-pass changeme
```

- `/admin/users` … 管理者向け画面。Basic認証で保護されており、ユーザ一覧の表示、新規登録、削除が行える。
- `/password` … 利用者向け画面。現在のパスワードで認証したうえで新しいパスワードを設定できる。

Web UIでの操作はSIPプロキシと同じSQLiteデータベースを利用するため、同じ資格情報でREGISTER認証を行える。
