This doc in English: [Open Match v2.x Ecosystem](README.md)

# Open Match v2.x エコシステム

このサブディレクトリには、Open Match v2.xをベースにしたマッチメーカーを構築するためのサンプルアプリケーションと開発フレームワークが含まれています。以下のディレクトリとファイルで構成されています。

* [`examples/`](examples/) ディレクトリには、Open Matchと連携するマッチメーカーアプリケーションの実装例が含まれています。詳細については、[`examples/README-JP.md`](examples/README-JP.md)（または英語版の [`examples/README.md`](examples/README.md)）を参照してください。
* [`internal/`](internal/) ディレクトリには、`examples/` ディレクトリ内のアプリケーションで共有されるGoパッケージ（`omclient`、`mmqueue`、`gsdirector`、`assignmentdistributor`、`extensions`、`logging`、`metrics`、および `mocks/gameclient`）が含まれています。
* [`cloudbuild.yaml`](cloudbuild.yaml) ファイルを使用すると、Google Cloud Buildpacksを利用して `examples/` ディレクトリ内の各アプリケーションのコンテナイメージをビルドできます。使用方法の詳細はファイル先頭のコメントに記載されており、コンテナイメージのデプロイ手順は各アプリケーションのディレクトリ内に記載されています。

## 開発 (Development)

### メンテナー向け (For Maintainers)

このモジュール（`open-match.dev/open-match-ecosystem/v2`）は **Go 1.26以降** を対象としています。`v2/` ディレクトリで依存関係をメンテナンスする際は、以下の手順に従ってください。

1. **Open Match Core (`om-core`) のprotobufおよびクライアントライブラリの更新**:
   ```bash
   cd v2
   go get github.com/googleforgames/open-match2/v2@latest
   ```
2. **直接および間接的なモジュール依存関係の更新と整理**:
   ```bash
   cd v2
   go get -u ./... && go mod tidy
   ```
3. **変更を提出する前のモジュール整合性・静的解析・単体テストの検証**:
   ```bash
   cd v2
   go mod verify
   go vet ./...
   go test -race -count=1 ./...
   ```

### ビルド (Build)

以下のコマンドはすべて（`go.mod` と `internal/` が配置されている）`v2/` ディレクトリから実行してください。

* **すべてのパッケージとサンプルバイナリのビルド**:
  ```bash
  go build ./...
  ```
* **静的解析の実行**:
  ```bash
  go vet ./...
  golangci-lint run ./...
  ```
* **レースディテクタを有効にした単体テストの実行**:
  ```bash
  go test -race -count=1 ./...
  ```
* **Google Cloud Buildを使用したコンテナイメージのリモートビルド**:
  `v2/` ディレクトリから [`cloudbuild.yaml`](cloudbuild.yaml) を使用し、ビルド対象のサンプルパッケージを `_GOOGLE_BUILDABLE` に指定します。
  ```bash
  # オールインワンのmatchmakerサンプルイメージをビルド
  gcloud builds submit --async \
    --substitutions=COMMIT_SHA=latest,_JOB_NAME=matchmaker-example,_GOOGLE_BUILDABLE=./examples/matchmaker

  # サンプルマッチメイキング機能（MMF）イメージをビルド
  gcloud builds submit --async \
    --substitutions=COMMIT_SHA=latest,_JOB_NAME=soloduel,_GOOGLE_BUILDABLE=./examples/mmf/
  ```

## デプロイと実行 (Deploy & Run)

* **[アーキテクチャおよび設計ガイド (`examples/README-JP.md`)](examples/README-JP.md)** ([英語版 (`examples/README.md`)](examples/README.md)): Open Match 2のサンプルコンポーネント間の連携、ゲームサーバーの可用性とマッチメイキングプロファイルの調整（リコンシリエーション）、バックフィルの優先順位付け、およびチケット衝突とレプリケーション遅延の見分け方・対処方法について解説しています。
* **[オールインワン・マッチメーカーハーネス (`examples/matchmaker`)](examples/matchmaker/README.md)**: ローカルでの迅速な反復開発やCloud Runでのシングルトンテスト向けに、`mmqueue`、`gsdirector`、およびプロセス内の `fifo` / `debug`（オプトイン）MMFサーバーを単一バイナリ内で実行します。ローカル実行手順、HTTP制御エンドポイント（`POST /tickets`、`GET/POST /tpc/{tixPerCycle}`、`GET /`）、環境変数、および [`examples/matchmaker/deploy/cloudbuild.yaml`](examples/matchmaker/deploy/cloudbuild.yaml) を用いたCloud Runへのデプロイ手順が含まれています。
* **[マッチメイキング機能（MMF）サンプル (`examples/mmf`)](examples/mmf/README.md)**: 環境変数 `MMF_FUNCTION` によって `fifo` と `debug` のマッチメイキング機能を切り替え可能なスタンドアロンgRPCサーバーです。MMF実装のベストプラクティスおよび [`examples/mmf/deploy/cloudbuild.yaml`](examples/mmf/deploy/cloudbuild.yaml) を用いたCloud Runへのデプロイ手順が含まれています。
* **[スタンドアロン・マイクロサービス (`examples/standalone`)](examples/standalone)**: 本番環境向けに、フロントエンドのマッチメイキングキュー（[`examples/standalone/mmqueue`](examples/standalone/mmqueue/README.md)）とバックエンドのゲームサーバーDirector（[`examples/standalone/gsdirector`](examples/standalone/gsdirector/README.md)）を分離した構成です。Google Cloud Pub/Sub（`ASSIGNMENT_DISTRIBUTION_PATH=pubsub`）を介して非同期にマッチアサインメントを配信します。
