# claude-oauth-proxy

OpenCode에서 Claude Code의 OAuth 인증을 사용할 수 있게 해주는 로컬 리버스 프록시.

## 개요

```
OpenCode → cc-proxy (localhost:34156) → api.anthropic.com
```

OpenCode의 Anthropic API 요청을 가로채서 Claude Code 호환 형식으로 변환 후 전달합니다.

## 변환 내용

| # | 변환 | 설명 |
|---|------|------|
| 1 | Auth 헤더 교체 | `x-api-key` → `Authorization: Bearer {CC OAuth token}` |
| 2 | 빌링 헤더 주입 | `x-anthropic-billing-header: cc_version=...` |
| 3 | Beta 헤더 병합 | `claude-code-20250219`, `oauth-2025-04-20` 추가, `context-1m` 제거 |
| 4 | 도구 이름 치환 | `todowrite` → `task_write`, `background_output` → `bg_output` |
| 5 | 시스템 프롬프트 | XML 태그 → Markdown 변환 |
| 6 | 응답 역치환 | 도구 이름을 원래대로 복원 (SSE 스트리밍 지원) |

## 요구사항

- Go 1.21+
- macOS (Keychain 접근용)
- Claude Code가 설치되어 있고 OAuth 로그인이 되어 있어야 함

## 빌드

```bash
go build -o cc-proxy .
```

## 사용법

### 1. 프록시 시작

```bash
./cc-proxy -listen 127.0.0.1:34156
```

### 2. OpenCode 실행

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:34156/v1 \
ANTHROPIC_API_KEY=proxy-placeholder \
opencode
```

또는 래퍼 스크립트 사용:

```bash
#!/bin/bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:34156/v1"
export ANTHROPIC_API_KEY="proxy-placeholder"
exec opencode "$@"
```

### 3. 헬스 체크

```bash
curl http://127.0.0.1:34156/health
```

## 토큰 관리

- macOS Keychain에서 Claude Code 자격증명 자동 읽기 (`Claude Code-credentials`)
- 파일 폴백: `~/.claude/.credentials.json`
- 만료 5분 전 자동 갱신
- 401 응답 시 토큰 갱신 후 1회 재시도

## 주의사항

- 첫 실행 시 macOS Keychain 접근 권한 다이얼로그 → **Always Allow** 선택
- `wss://bridge` (SDK 경로)가 아닌 직접 HTTP (`api.anthropic.com`)로 전송
- 개인 사용 목적의 로컬 프록시 — 프로덕션 용도가 아님

## 테스트

```bash
go test -v ./...
```

## 라이선스

MIT
