# mediamtx-hls-proxy

GoでTLS終端しながら、複数ドメインごとにMediaMTXのHLSをHTTPSでプロキシする軽量サーバーです。

## できること

- SNIで複数ドメインの証明書を切り替え
- ドメインごとに別のMediaMTX HLSパスへリバースプロキシ
- HTTPSポートだけで待ち受け
- `/` 配下でplaylistとsegmentを同一オリジン配信
- `/metrics/` でPrometheus形式のmetricsを公開
- MPEG-TS segmentをメモリキャッシュしてMediaMTXへの負荷を削減
- ドメインごとの証明書ファイルパスを設定可能

## 前提

- DNSで各ドメインがこのサーバーを向いていること
- 各ドメインの証明書と秘密鍵をこのサーバー上で参照できること
- MediaMTX側でHLSが有効になっていること

## 設定

1. [config.example.json](config.example.json) を `config.json` にコピーして編集します。
2. 各 `domains[].upstream` に、対象のMediaMTX HLSベースURLを指定します。
   例: `http://127.0.0.1:8888/camera1/`
3. 各ドメインに `cert_file` と `key_file` を設定します。
4. metricsの公開パスを変えたい場合は `metrics_path` を変更します。
5. `cache_max_bytes` はメモリキャッシュ上限です。デフォルトは 512MB です。
6. `cache_ttl_seconds` はMPEG-TS segmentのキャッシュ保持秒数です。

### `config.json` の例

```json
{
  "listen_https": ":443",
  "metrics_path": "/metrics/",
  "cache_max_bytes": 536870912,
  "cache_ttl_seconds": 30,
  "domains": [
    {
      "host": "cam1.example.com",
      "upstream": "http://127.0.0.1:8888/camera1/",
      "proxy_path": "/",
      "cert_file": "C:/certs/cam1.example.com/fullchain.pem",
      "key_file": "C:/certs/cam1.example.com/privkey.pem"
    }
  ]
}
```

## ビルド

Linux版:
```bash
go build -o ./bin/mediamtx-hls-proxy-linux-amd64 .
```

Windows版:
```powershell
go build -o ./bin/mediamtx-hls-proxy.exe .
```

## 起動

```bash
./bin/mediamtx-hls-proxy-linux-amd64
```

`-config` を省略した場合は、バイナリと同じディレクトリにある `config.json` を読み込みます。

```powershell
./mediamtx-hls-proxy-linux-amd64 -config config.json
```

相対パスで `-config` を指定した場合も、カレントディレクトリではなくバイナリ配置ディレクトリ基準で解決します。

## Ubuntu での `systemd` sample

サンプルは [`deploy/systemd/mediamtx-hls-proxy.service`](deploy/systemd/mediamtx-hls-proxy.service) に置いてあります。

1. バイナリと `config.json` を `/opt/mediamtx-hls-proxy/` に配置
2. service ファイルを `/etc/systemd/system/mediamtx-hls-proxy.service` にコピー
3. 必要に応じて `User`、`Group`、`ExecStart`、証明書パスを環境に合わせて変更
4. 以下を実行

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mediamtx-hls-proxy
sudo systemctl status mediamtx-hls-proxy
```

ログ確認:

```bash
journalctl -u mediamtx-hls-proxy -f
```

## GitHub Actions

以下を追加済みです。

- `.github/workflows/build.yml`  
  push / pull request / 手動実行で `go test ./...` と Linux向けビルドを実行し、artifact を保存します。
- `.github/workflows/update-deps.yml`  
  毎週自動で `go get -u ./...` → `go mod tidy` → build を行い、差分があればPRを作成します。
- `.github/dependabot.yml`  
  Go modules と GitHub Actions 自体のバージョン更新を weekly でチェックします。

> `update-deps.yml` で自動PRを作るため、GitHub リポジトリの **Settings > Actions > General > Workflow permissions** を **Read and write permissions** にしておくと確実です。

## 使い方

- `https://cam1.example.com/index.m3u8` のように `/` 配下でplaylistを直接確認できます。
- segmentやpartial segmentも `/` 配下で同じようにプロキシされます。
- `https://cam1.example.com/metrics/` でPrometheus形式のmetricsを取得できます。
- `hls_viewers{stream="camera1"}` で、直近30秒以内にHLSを取りに来たユニークIP数を確認できます。
- `.ts` と `.mpegts` のGETリクエストだけがメモリキャッシュ対象です。

## 注意点

- MediaMTXが認証付きなら、このプロキシ側に認証ヘッダー付与などの追加実装が必要です。
- `/metrics/` は各ドメインで共通に公開され、このパスだけは上流へ流さずローカルのmetricsを返します。外部公開したくない場合はFWやリバースプロキシで制限してください。
- 取得できる主な指標はリクエスト総数、レスポンスステータス総数、処理時間、同時処理数、ストリームごとの推定視聴者数、キャッシュヒット数、キャッシュミス数、キャッシュ使用量です。
- playlist はキャッシュしません。ライブ性を落とさず、MPEG-TS segment だけを軽くする構成です。