# Upstream reference (SPEC-v3 V14.6)

The Go code in this package is a reimplementation, not a copy: no upstream source
was vendored or translated line by line. It follows the protocol of

- **google-colab-cli 0.7.5** (Apache-2.0, © 2026 Google LLC), PyPI wheel
  `google_colab_cli-0.7.5-py3-none-any.whl`, read 2026-10-24 (not installed, not run);
- **jupyter-kernel-client 0.9.0** (BSD-3-Clause, © 2023-2024 Datalayer, Inc. in the source headers; the wheel's LICENSE file reads "Copyright (c) 2025, Datalayer"), the
  version google-colab-cli 0.7.5 pins, for the Jupyter kernel REST and WebSocket
  protocol.

When upstream changes, compare the files below and update the samples in
`testdata/replay/` (each sample names the upstream file and function it follows).

| Go file | Follows (upstream file → function / class) | Notes |
|---|---|---|
| `credential.go` | `colab_cli/auth.py` → `PUBLIC_SCOPES`, `_get_adc_credentials` (ADC discovery, required scopes, the `gcloud auth application-default login --scopes=…` command) | Only ADC; the OAuth2 copy-paste flow (`_run_remote_flow`) and the bundled client config are not used. Refresh requests narrow the scopes to `openid userinfo.email colaboratory` (upstream re-applies `PUBLIC_SCOPES`). Revocation (RFC 7009) has no upstream counterpart. |
| `api.go` | `colab_cli/client.py` → `Prod`, `Client._issue_request` (headers, `authuser=0`, XSSI prefix), `Client.assign`, `_build_assign_url`, `_get_assignment`, `_post_assignment`, `Client.unassign`, `Client.list_assignments`, `Client.get_consumption_user_info`, `uuid_to_web_safe_base64`, enums `Accelerator`, `Variant`, `AssignmentVariant`, `Shape`, models `RuntimeProxyInfo`, `ListedAssignment`, `PostAssignmentResponse`, `GetAssignmentResponse`, `Assignment`; `colab_cli/consumption.py` → `CcuInfo` | Upstream retries 502/503/504 and read timeouts three times (10 s × 2ⁿ) and gives up on 412; LectureFlow's caller treats 412 and transient failures as "waiting for an accelerator" with its own back-off (30 s doubling to 10 min) up to the user's limit. `X-Colab-Client-Agent` is `lectureflow` (upstream: `colab-cli`). |
| `session.go` | `colab_cli/commands/session.py` → `new`, `stop`, `resolve_runtime_options`; `colab_cli/common.py` → `State.get_session` (proxy token refresh through `list_assignments`, `TOKEN_REFRESH_MARGIN` 5 min), `State.sync_sessions` (records not in the assignment list are gone); `colab_cli/commands/run.py` → `_teardown` | Startup cleanup of a crashed run's session only touches the endpoint (or notebook hash) LectureFlow recorded, never other sessions of the account. |
| `runtime.go` | `colab_cli/contents.py` → `ContentsClient` (`upload`, `download`, `rm`, token in `colab-runtime-proxy-token`); `colab_cli/runtime.py` → `ColabRuntime.kernel_client` (headers `X-Colab-Runtime-Proxy-Token`, kernel kept alive), `execute_code`; jupyter-kernel-client 0.9.0 `manager.py` → `fetch`, `KernelHttpManager.start_kernel` (`{"name":"python3","path":null}`), `list_kernels`, `shutdown_kernel`; `wsclient.py` → `KernelWebSocketClient.start_channels` (`session_id`, `token`), default subprotocol; `utils.py` → `deserialize_msg_from_ws_default`, `serialize_msg_to_ws_json`; `client.py` → `output_hook` | Uploads larger than one chunk continue with chunk 2, 3, …, -1 (JupyterLab's protocol; upstream always sends one chunk). Downloads ask for `format=base64&type=file`. |
| `ws.go` | (websocket-client, used by jupyter-kernel-client) | Minimal RFC 6455 client written here; no new module dependency. Ping every 30 s (upstream `ping_interval` 60 s). |
| `job.go` | `colab_cli/commands/run.py` → `run_command`, `_exit_code_from_outputs` (the script runs as one cell; `SystemExit` → exit code); README "Activity-Based Liveness" (a busy kernel keeps the runtime) | The cell runs `worker/colab_remote.py` as a subprocess and copies only its stdout lines; stderr goes to a remote file that is never downloaded. |
| `events.go` | — (LectureFlow's own contract, V14.3 step 3) | Same tables as `worker/colab_remote.py` (`EVENT_KEYS`, `STAGES`, `ERROR_CODES`), checked by `worker/test_colab_remote.py::test_contract_tables_match_go`. |
| `estimate.go` | — (LectureFlow's fair-use rule, V14.3 step 4) | Default compute-unit rates are unofficial estimates, replaced by `ccu-info`'s `consumptionRateHourly` while a session runs. |
| `record.go`, `cmd/record` | — | Records real exchanges in the replay format with secrets and texts redacted. |

Not used from upstream: TPUs, high-RAM by default, Drive mount, VM-side `colab auth`,
`console` (tmux terminal) and `ssh` (no remote-control connection types, V14.1),
history logs, notebook export, auto-update.
