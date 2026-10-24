This doc in English: [Open Match 2 Examples](README.md)  
GitHubレポ: [https://github.com/googleforgames/open-match-ecosystem/tree/main/v2](https://github.com/googleforgames/open-match-ecosystem/tree/main/v2)

# Open Match 2 サンプルマッチメーカーのアーキテクチャ概要

`v2/` ディレクトリにあるサンプルマッチメーカーは、Open Match 2を使用して完全なマッチメイキングシステムを構築する方法を示します。ローカル開発用のモノリシックなデプロイメントと、本番環境で推奨される分離されたマイクロサービスベースのアーキテクチャの両方をサポートするように、柔軟に設計されています。

#### **コアコンポーネントとワークフロー**

このシステムは主に `examples/` ディレクトリにあるアプリケーションで構成されており、これらは `internal/` ディレクトリの共有ロジックを活用しています。

**1. アプリケーション (`examples/` ディレクトリ):**

* **[matchmaker](matchmaker/README.md)**: すべてのコンポーネントを単一のプロセスで実行する開発およびテスト用のハーネスです。メモリ内の `mmqueue`、`gsdirector`、およびローカルのマッチメイキング機能（MMF）サーバー（`fifo` と `debug`）を初期化するため、複数のサービスをデプロイすることなくローカル開発を行うのに理想的です。
* **[standalone/mmqueue](standalone/mmqueue) (マッチメイキングキュー):** ゲームクライアント向けのフロントエンドサービスです。
  * **機能:** クライアントからのマッチメイキングリクエストを受け取り、Open Matchの `Ticket` protobufを作成し、`omclient` を介して `om-core` での作成と有効化のためにキューに入れます。また、Directorから最終的なゲームサーバーのアサインメントを受け取り、クライアントに返します。
* **[standalone/gsdirector](standalone/gsdirector) (ゲームサーバーDirector):** ゲームサーバーマネージャーとのマッチメイキングプロセスを調整するバックエンドサービスです。
  * **機能:** ゲームサーバーマネージャー（この例ではモックのAgonesインテグレーション）にクエリを送信し、どのような種類のマッチが必要かを判断します。次に `Profile` オブジェクトを構築し、`om-core` の `InvokeMatchmakingFunctions` エンドポイントを呼び出します。`Match` の提案を受け取った後、それらを評価し、承認されたマッチにゲームサーバーを割り当て、接続詳細を `mmqueue` に送信します。
* **[mmf](mmf/README.md) (マッチメイキング機能):** カスタムのゲーム固有のマッチングロジックを含む、スタンドアロンのgRPCサーバーです。
  * **機能:** `om-core` はこのサービスの `Run` メソッドを呼び出し、フィルタリングされたチケットのプールを含む `Profile` をストリーミングします。MMFは自身のロジック（例：提供されている `fifo` 関数は単純な先入れ先出しのマッチを作成し、`debug` 関数は受信したプロファイルとプールをログ出力）を実行し、`Match` の提案をストリームで返します。

**2. 共有ロジック (`internal/` ディレクトリ):**

* **[omclient](../internal/omclient)**: `om-core` サービスのgRPC-Gatewayエンドポイントと通信するためのRESTful HTTPクライアント。チケットの作成、チケットごとの結果通知を伴うバッチ有効化、マッチメイキング機能の呼び出しを処理します。
* **[mmqueue](../internal/mmqueue)**: マッチメイキングキューのコアロジックを含むモジュール。クライアントリクエストの管理やアサインメントの処理が含まれます。モノリシックな `matchmaker` の一部として、またはスタンドアロンの `mmqueue` サービスとして実行できます。
* **[gsdirector](../internal/gsdirector)**: ゲームサーバーDirectorのロジックを含みます。このモジュールには `MockAgonesIntegration` が含まれており、特定のリージョンで利用可能なゲームモードなどのマッチメイキング要件を提供し、モックのサーバー割り当てを処理することで、ゲームサーバーマネージャーをシミュレートします。
* **[assignmentdistributor](../internal/assignmentdistributor/README.md)**: サンプルがゲームサーバーDirectorからマッチメイキングキューにアサインメントを返すために使用するインターフェース、および2つの実装（1つは本番環境に近い分離構成向けにGoogle Cloud Pub/Subを使用、もう1つはプロセス内のローカル開発用にGoチャネルを使用）。
* **[metrics](../internal/metrics) & [logging](../internal/logging)**: すべてのアプリケーションで使用される、OpenTelemetryメトリクスと構造化されたLogrusロギングを初期化するための中央集権的なモジュールです。
* **[extensions](../internal/extensions)**: Open Matchのprotobufメッセージの `extensions` マップで広く使用される `google.protobuf.Any` 型を扱うためのユーティリティ関数と標準キー定義を提供します。
* **[mocks/gameclient](../internal/mocks/gameclient)**: テスト目的でゲームクライアントからのリクエストをシミュレートする、モックの `Ticket` オブジェクトを生成するユーティリティです。

### **マッチメーカーの設計解説**

Open Match v2サンプルマッチメーカーの設計は、3つの主要な情報を調整（リコンサイル）することを中心に展開されます。それは、特定のゲームモードに対して**プレイヤーをどのようにマッチングさせるか**のルール、**利用可能なゲームサーバー**のインベントリとそれらがホストできるマッチの種類、そしてマッチを待っている**利用可能なプレイヤー**のプールです。

#### **単一の信頼できる情報源（Single Source of Truth）としてのゲームサーバー（サーバーの可用性とマッチロジック）**

本番環境では、サーバーの可用性に関する情報とマッチ作成のルールは連携しており、ゲームサーバー自体が単一の信頼できる情報源として機能します。

* **推奨される本番設計**: `GameMode`（マッチメイキングルール、プレイヤーのプール、MMFパラメータを含む）の定義は、マッチメーカーにハードコードするのではなく、TerraformやHelmのようなInfrastructure-as-Code（IaC）ツールを通じて管理されます。デプロイ時に、これらの `GameMode` 仕様はAgones Fleetなどのゲームサーバーテンプレートにメタデータ（例：Kubernetesアノテーション）として適用されます。その結果、そのFleetから作成されたすべての `GameServer` インスタンスは、ホストするように設計されたマッチの正確な定義を自動的に継承します。
* **サンプルでのシミュレーション方法**: サンプルマッチメーカーは、[`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go) 内の **`MockAgonesIntegration`** を使用してこの本番設計をシミュレートします。
  * 静的な `GameMode`、`FleetConfig`、`ZonePools` 変数は、IaCツールに存在するであろう設定の代役です。
  * `MockAgonesIntegration.Init()` 関数はデプロイプロセスをシミュレートし、これらの静的設定を消費してメモリ内に `gameServers` のマップを生成します。このマップ内の各モックサーバーセットは、それが必要とする特定の `MmfRequest` に関連付けられます。
  * 次に、Directorは **`GetMMFParams()`** を呼び出します。実際のシステムでは、この関数はKubernetes APIにクエリを送信して準備完了状態の `GameServer` を検出し、そのマッチメイキングメタデータを直接読み取ります。このサンプルでは、初期化時に作成されたメモリ内のマップから単純に読み取るだけです。
* このアプローチにより、Directorは常にゲームサーバーインフラの最新の状態に基づいて動作し、別途設定を必要とせずに、どのような種類のマッチが必要かを自動的に検出できます。

#### **利用可能なプレイヤー情報の保存**

この責務は、マッチメイキングキューとOpen Match Coreの連携によって担われます。

* **マッチメイキングキュー (`mmqueue`)**: このサービス ([`v2/internal/mmqueue/mmqueue.go`](../internal/mmqueue/mmqueue.go)) はプレイヤーにとっての入り口です。プレイヤーからのマッチ検索リクエストを受け付け、Open Matchの `Ticket` オブジェクトを作成します。
* **Open Match Core (`om-core`)**: `om-core` は権威あるステートストアです。`mmqueue` は `om-core` 内でチケットを作成・有効化し、`om-core` は現在マッチを待っているすべてのプレイヤーのクエリ可能な確定的なプールを維持します。

### **リコンシリエーション（調整）プロセス**

システムの主要な機能は、まずサーバーの可用性とそれがホストできるゲームモードのルールを調整し、どのようなプレイヤーが必要かを理解することです。次に、そのニーズを利用可能なプレイヤーのプールと照合して最適なマッチを作成し、適切なサーバーに割り当てます。

#### **サーバーニーズの調整**

* **担当コンポーネント**: ゲームサーバーDirector **(`gsdirector`)**。
* **プロセス**: `gsdirector` のメインループは、ゲームサーバーマネージャー（`MockAgonesIntegration.GetMMFParams()` によってシミュレート）にクエリを送信することから始まります。これにより、利用可能なゲームサーバーのメタデータから直接 `MmfRequest` オブジェクトが検出されます。この最初のステップで、サーバーの可用性とマッチ要件が調整され、実行すべき具体的なマッチメイキングタスクのリストが生成されます。

#### **プレイヤーとサーバーニーズの調整**

* **担当コンポーネント**: **`gsdirector`**、**`om-core`**、および **`mmf`** の連携。
* **プロセス**:
  * `gsdirector` は検出した `MmfRequest` を `om-core` に送信します。
  * `om-core` は、各リクエストの `Pools` 内の基準に基づいて、利用可能なプレイヤーのアクティブチケットキャッシュをフィルタリングします。
  * 次に、`om-core` は適切な**マッチメイキング機能 (`mmf`)** を呼び出し、フィルタリングされたプレイヤーのプールをストリーミングします。
  * `mmf` はゲーム固有のロジックを実行し、`Match` の提案をストリームで返します。
  * 最後に、`gsdirector` はこれらの提案を受け取り、最終検証を行い、承認された各マッチに特定の準備完了状態のゲームサーバーを割り当て、プレイヤーからサーバーへのアサインメントを完了させます。([`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go))

### **高度なマッチメーカー操作：バックフィルとチケット衝突の処理**

主要なマッチメイキングフローに加えて、堅牢なシステムは、部分的に空のゲームサーバーを埋める（バックフィル、途中参加、高密度ゲームサーバーなど）や、同じプレイヤーが同時に複数のマッチに提案されるケースを解決する（チケット衝突）といった一般的なシナリオを処理する必要があります。

#### **バックフィルの処理**

サンプルマッチメーカーは、バックフィルを、MMFが満員でないマッチを作成した際に生成される、特定の高優先度のマッチメイキングリクエストとして扱います。

* **バックフィルリクエストの生成 (MMF内)**:
  * プロセスは [`v2/examples/mmf/functions/fifo/fifo.go`](mmf/functions/fifo/fifo.go) などのMMFで始まります。MMFがマッチを形成するものの、利用可能なチケットプールから全てのプレイヤースロットを埋めることができない場合、そのマッチを不完全と判断します。
  * `om-core` にマッチ提案を返す前に、MMFは `Profile.Name` に `"backfill."` プレフィックス（`"backfill." + match.GetId()`）を付与した*新しい* `MmfRequest` を構築します。この新しいリクエストには、空のスロットを埋めるために必要な残りのプレイヤー数を見つけるために調整された新しい `Profile` が含まれています。
  * この新しいバックフィル `MmfRequest` は、元の `Match` 提案の `Extensions` フィールドに `open-match.dev/mmf-request` キーとして添付されます。([`v2/examples/mmf/functions/fifo/fifo.go`](mmf/functions/fifo/fifo.go))
* **バックフィルリクエストの処理 (Director内)**:
  * `gsdirector` はMMFからマッチ提案を受け取り、その `Extensions` を検査します。有効なバックフィルリクエスト（空でない `Profile.Name` を持つもの）が添付されている場合、このマッチをホストするゲームサーバーが後続サイクルでさらにプレイヤーを必要とすることを知ります。
  * 次に、Directorはゲームサーバーマネージャー（`MockAgonesIntegration`）の `UpdateMMFRequest` を呼び出し、割り当てられたゲームサーバーのメタデータを更新して新しいバックフィル要件を反映させます。その後 `Allocate` が呼び出された際、`MockAgonesIntegration` はこのサーバーに紐付けられたバックフィル `MmfRequest` を消去せずに保持します。([`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go))
  * **優先順位付け**: Directorは以下の2段階でバックフィルリクエストを優先します。
    1. **リクエスト送信順序の優先化**: `GetMMFParams()` がゲームサーバーからマッチメイキングリクエストを収集する際、同じ割り当て仕様（AllocationSpec）を共有するサーバー数の昇順でソートします。バックフィルリクエストは単一のサーバーに固有（参照カウント `1`）であるため、リクエストリストの先頭に配置され、最初に `om-core` へ送信されます。また、`WAIT_FOR_BACKFILL_MMFS=true` が設定されている場合、Directorは `"backfill."` プロファイルの `InvokeMatchmakingFunctions` 呼び出しが完了するのを待ってから通常のMMFリクエストを並行起動します。
    2. **マッチ提案評価順序の優先化**: `ProposalsLoop` を実行する前に、Directorは返却されたすべての `proposedMatches` を安定ソート（`sort.SliceStable`）し、`profileName` 拡張が `"backfill."` で始まる提案を通常のマッチ提案より先に評価します。([`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go))

#### **チケット衝突の処理**

Open Match 2は複数のMMFやプロファイルを並行して実行でき、またチケットの非有効化（deactivation）状態更新はRedisを介して `om-core` インスタンス間で非同期にレプリケーションされるため、同じ `Ticket` が同一サイクル内または連続するサイクル間で複数の `Match` 提案に含まれる可能性があります。これらの「チケット衝突」を検出し解決する責任はDirector（[`v2/internal/gsdirector/gsdirector.go`](../internal/gsdirector/gsdirector.go)）にあります。

* **サイクル間衝突と同一サイクル内衝突の検出**:
  * **サイクル間の割り当て済みチケット追跡 (`assignments` マップ)**: `gsdirector` は、過去のサイクルで既にゲームサーバーが割り当てられアサインメントが送信されたすべてのチケットIDを記録する `assignments` マップ（エントリは `ASSIGNMENT_TTL_MS` 経過後に期限切れ）を維持します。提案されたマッチに `assignments` 内のチケットが含まれている場合（`numAssigned > 0`）、以前のサイクルで割り当てられたチケットが次のサイクル開始時点でまだ `om-core` インスタンスのローカルキャッシュ内でアクティブに見えていたことを意味します。このマッチは拒否され、`gsdirector.matchmaking_cycle.match.rejections.assigned` および `gsdirector.ticket.rejections.assigned` メトリクスに記録されます。
  * **同一サイクル内の承認済みチケット追跡 (`acceptedTicketsThisCycle` マップ)**: 各マッチメイキングサイクル内で、`gsdirector` はサイクル固有の `acceptedTicketsThisCycle` マップも初期化します。`ProposalsLoop` でマッチ提案が検証を通過するたびに、そのマッチ内のすべてのチケットIDが `acceptedTicketsThisCycle` に登録されます（後続のサーバー割り当てが失敗した場合は削除されます）。同一サイクル内の後続のマッチ提案に `acceptedTicketsThisCycle` に既に存在するチケットが含まれている場合（`numSameCycleOverlap > 0`）、Directorは同一サイクル内のMMF・プロファイル重複（オーバーラップ）と判定してそのマッチを拒否します（この場合、`gsdirector.matchmaking_cycle.match.rejections` は増加しますが、`gsdirector.matchmaking_cycle.match.rejections.assigned` は増加しません）。
* **衝突解決時における `backfill.` 提案の優先化**:
  * `proposedMatches` は `"backfill."` で始まるプロファイル名の提案が先になるよう安定ソートされるため、バックフィルマッチが最初に `acceptedTicketsThisCycle` へチケットを登録します。同一サイクル内でバックフィルMMFと通常のMMFが同じプレイヤーを提案した場合、バックフィルマッチが優先的に承認され、競合した通常のマッチ提案が拒否されます。
* **拒否されたマッチのチケット再有効化時における割り当て済み・承認済みチケットのスキップ**:
  * マッチ提案が何らかの理由で拒否された場合、そのマッチに含まれる未割り当てのチケットは `om-core` の `ActivateTickets` エンドポイント（`rejectMatch` 関数）を介してアクティブなマッチメイキングプールに戻す必要があります。
  * しかし、拒否されたマッチには、未割り当てのチケットだけでなく、過去のサイクルで既に割り当て済みのチケット（`assignments[tid]`）や、同一サイクル内の別のマッチで既に承認済みのチケット（`acceptedTicketsThisCycle[tid]`）が混在している場合があります。
  * `rejectMatch` では、`if assignments[tid] || acceptedTicketsThisCycle[tid]` を明示的にチェックし、既に割り当て済みまたは同一サイクル内で承認済みのチケットIDを**スキップ**して、まだどのマッチにも確保されていないチケットID**のみ**を `ticketIdsToActivate` チャネルへ送ります。これにより、既に有効なマッチが確定したプレイヤーが誤って `om-core` で再有効化されることを防ぎます。

#### **om-coreによる支援と、レプリケーション遅延とMMF重複の見分け方**

`om-core` と `gsdirector` は連携してサイクル間のチケット衝突を最小限に抑えるとともに、**Redisキャッシュのレプリケーション遅延**と**同一サイクル内のMMF重複**を明確に切り分けるためのメトリクスを提供します。

* **`om-core` におけるチケット非有効化の確認待ち**:
  * MMFが `Match` 提案を `om-core` にストリーム返却すると、`om-core` はそのマッチのロスター内のすべてのチケットを非有効化（deactivation）する状態更新を自動的にRedisのレプリケーションストリームへキューイングし、後続の `InvokeMatchmakingFunctions` 呼び出しにそれらのチケットが含まれないようにします。
  * **`OM_MATCH_TICKET_DEACTIVATION_WAIT`** が `true`（`om-core` のデフォルト）の場合、`om-core` はそれらの非有効化更新がRedisストリームを経由して自身のローカルインメモリキャッシュに反映されるまで、最大 **`OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`**（デフォルト `60000` ミリ秒）待機してから `Match` をDirectorへストリーム返却します。
  * **`OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS`** 以内にローカルキャッシュでの非有効化確認が完了しなかった場合、`om-core` は `om_core.mmf.deactivation.timeouts` をインクリメントし、`Match.Extensions["deactivation_timeout"]` にブール値の拡張 **`deactivation_timeout`**（`true`）を付与した上で、そのマッチをDirectorへ返却します。
* **Directorのポリシー (`REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT`) と適応型バックオフ**:
  * `gsdirector` は受信した各マッチの `deactivation_timeout` 拡張を検査し（`hasDeactivationTimeout`）、`gsdirector.matchmaking_cycle.match.deactivation_timeouts` および `gsdirector.matchmaking_cycle.ticket.deactivation_timeouts` を記録します。
  * **`REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT=false`**（デフォルト）の場合、`gsdirector` は警告ログを出力しつつ、他の検証チェックを通過すればそのマッチを承認し、次サイクルまでのスリープ倍率（`computeCycleBackoffFactor`：`max(numMatchesWithAssignedTickets, numMatchesWithDeactivationTimeout) / numProposed + 1.0`）を動的に引き上げて、`om-core` のレプリケーションが追いつく時間を確保します。
  * **`REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT=true`** の場合、`gsdirector` は `deactivation_timeout=true` が付与されたマッチを直ちに拒否し、未割り当てのチケットを再有効化した上で、適応型サイクルバックオフも適用します。

**診断テーブル：`gsdirector` / `mmqueue` メトリクスと `om_core` メトリクスの対応**

| シナリオ | `gsdirector` / `mmqueue` メトリクス | `om_core` メトリクス | 診断結果と意味 |
| :--- | :--- | :--- | :--- |
| **1. サイクル間衝突（レプリケーション遅延）** | `gsdirector.matchmaking_cycle.match.rejections.assigned` > 0<br>`gsdirector.ticket.rejections.assigned` > 0 | `om_core.cache.replication.lag` の上昇（高いp95/p99）<br>`om_core.cache.incoming.backlog` / `om_core.cache.outgoing.backlog` > 0 | 前のサイクルで割り当てられたチケットの非有効化が、次のサイクル開始時までにすべての `om-core` インスタンスのローカルキャッシュへ伝播していなかったことを示します。 |
| **2. 同一サイクル内衝突（並行MMFの重複）** | `gsdirector.matchmaking_cycle.match.rejections` > 0<br>`gsdirector.matchmaking_cycle.match.rejections.assigned` == 0<br>`gsdirector.matchmaking_cycle.match.deactivation_timeouts` == 0 | `om_core.cache.replication.lag` は低く健全<br>`om_core.mmf.deactivation.timeouts` == 0 | 同一サイクル内で並行実行された複数のMMFまたはプロファイルが同じチケットを選択し、Director内の `acceptedTicketsThisCycle` によってローカルで衝突が解決されたことを示します。 |
| **3. 非有効化確認のタイムアウト** | `gsdirector.matchmaking_cycle.match.deactivation_timeouts` > 0<br>`gsdirector.matchmaking_cycle.ticket.deactivation_timeouts` > 0<br>`gsdirector.matchmaking_cycle.duration` の増加 | `om_core.mmf.deactivation.timeouts` > 0<br>`om_core.mmf.deactivation.wait` が `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` に接近 | `om-core` が `OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` 以内にマッチ内チケットのローカル非有効化を確認できず、`deactivation_timeout=true` を付与したことを示します。 |
| **4. チケット有効化・再有効化の失敗** | `mmqueue.ticket.activations.failures` > 0<br>`gsdirector.ticket.reactivations.failures` > 0 | `om_core.ticket.activation.failures.invalid_id` > 0<br>`om_core.ticket.activation.failures.unspecified` > 0<br>`om_core.rpc.errors` > 0 | `invalid_id` > 0 の場合はMMFが架空または破損したチケットIDをロスターに入れたことを示し、`unspecified` > 0 の場合は `ActivateTickets` 中にRedisステートストアエラーが発生したことを示します。 |
| **5. MMF呼び出し・ストリームの失敗** | `gsdirector.mmf.response.failures` > 0<br>`mmqueue.ticket.creations` > 0 なのに `gsdirector.matchmaking_cycle.match.proposals` が 0 に低下 | `om_core.mmf.failures` > 0<br>`om_core.mmf.deactivations` が停滞したまま `om_core.cache.tickets.active` が増加 | MMFがタイムアウト（`OM_MMF_TIMEOUT_SECS`）したか、gRPCエラーを返したか、`om-core` から到達不能であることを示します。 |

**対処手順 (Remediation Steps)**:

1. **パックされた状態更新 (`OM_CACHE_PACK_TICKET_STATE_UPDATES`) によるRedisレプリケーション負荷の削減**:
   * `om-core` で `OM_CACHE_PACK_TICKET_STATE_UPDATES=true` を有効にすると、複数のチケット有効化・非有効化更新が少数のRedisストリームエントリ（`om_core.cache.outgoing.updates`）にパックされ、高スループット時のRedis書き込み・読み取り負荷とレプリケーション遅延が大幅に削減されます。
   * **安全なロールアウト順序**: 古いバージョンの `om-core` インスタンスは、パックされた複数チケットのストリームエントリを解析できません。必ず以下の2段階でロールアウトしてください。
     1. まず、`OM_CACHE_PACK_TICKET_STATE_UPDATES=false` の状態で、パック形式の読み取りに対応した新しい `om-core` バイナリをすべてのインスタンスにデプロイします（またはメンテナンスウィンドウ中にRedisをフラッシュして古いインスタンスがストリームを読まないようにします）。
     2. 稼働中のすべての `om-core` インスタンスがアンパックに対応した後に、`OM_CACHE_PACK_TICKET_STATE_UPDATES=true` に更新します。
2. **非有効化待機時間とサイクル間隔の調整**:
   * `om-core` で `OM_MATCH_TICKET_DEACTIVATION_WAIT=true` を維持し、`OM_MATCH_TICKET_DEACTIVATION_TIMEOUT_MS` がピーク時の `om_core.cache.replication.lag` より大きくなるよう設定します。
   * `gsdirector` の `MM_CYCLE_MIN_DURATION_MS` を `om_core.cache.replication.lag` のp95/p99（および `OM_CACHE_IN_WAIT_TIMEOUT_MS`）より大きく設定し、遅延スパイク時に未確認マッチの通過よりも衝突防止を優先したい場合は `REJECT_MATCHES_ON_DEACTIVATION_TIMEOUT=true` を有効にします。
   * それでも `om_core.cache.replication.lag` が高い場合は、`om-core` インスタンスをスケールアウト（Cloud Runではリクエスト間もバックグラウンドのレプリケーションゴルーチンがスロットルされないよう `--no-cpu-throttling` を必ず指定）するか、Redisのリードレプリカを追加します。
3. **同一サイクル内のMMF重複の削減**:
   * 並行実行されるプロファイル間で `Pools` のフィルタ条件を分割して同じプレイヤー層を奪い合わないようにするか、`gsdirector` で `WAIT_FOR_BACKFILL_MMFS=true` を有効にして、通常のプロファイルが実行される前に `"backfill."` プロファイルのチケット非有効化を完了させます。
