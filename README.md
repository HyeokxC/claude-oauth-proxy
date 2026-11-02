# claude-oauth-proxy

Claude Code의 OAuth 자격증명을 사용해 OpenCode 요청을 Anthropic API로 전달하는 로컬 리버스 프록시입니다.

> **비공식 프로젝트**
>
> Anthropic과 제휴하거나 Anthropic의 보증을 받지 않습니다. 개인적인 로컬 사용만을 위한 도구이며, 프로덕션 서비스나 공개 프록시로 사용하지 마세요.

## 동작 방식

```text
OpenCode -> cc-proxy (127.0.0.1:34156) -> api.anthropic.com
```

프록시는 로그인 기능을 제공하지 않습니다. 기존 Claude Code 자격증명을 읽고 필요할 때 갱신합니다.

- macOS Keychain에서 현재 사용자 계정에 맞는 `Claude Code-credentials` 항목을 먼저 읽습니다.
- `CLAUDE_CONFIG_DIR`가 설정되어 있으면 해당 설정 디렉터리에 맞는 계정과 Keychain 서비스 이름을 사용합니다.
- Keychain을 읽지 못하면 설정 디렉터리의 `.credentials.json` 파일을 사용합니다.
- 토큰이 만료되기 5분 전 갱신하고, upstream이 401을 반환하면 새 토큰으로 한 번 재시도합니다.
- 매 호출과 갱신 실패 후에 Keychain 우선 순서로 저장소를 다시 선택해 재로그인과 파일→Keychain 전환을 반영합니다. 같은 자격증명의 갱신 실패는 30초 동안 재시도하지 않지만, 대기 중에도 새 로그인은 바로 반영됩니다.
- 갱신 결과는 원래 Keychain 항목이나 파일에 저장하고, JSON의 중첩 구조와 알 수 없는 메타데이터를 유지합니다. 파일의 심볼릭 링크는 유지하고 실제 대상 디렉터리에서 권한 `0600`의 임시 파일을 동기화한 뒤 교체하며, 디렉터리도 동기화·닫기까지 확인합니다. 저장 전 링크 대상 변경도 검사합니다. Keychain은 기존 항목만 네이티브 API로 수정하며, 실패해도 평문 파일로 전환하지 않습니다.
- 저장에 실패하면 새 자격증명과 원래 저장 대상을 메모리에 보관하고 오류를 반환합니다. 다음 호출은 갱신 요청 대신 저장·디렉터리 동기화를 재시도하며, 새 로그인이 나타나면 이를 우선합니다. 저장소만 바뀌고 인증 내용이 그대로라면 새 토큰을 버리거나 다른 저장소에 쓰지 않고, 원래 저장소가 다시 활성화되거나 새 로그인이 확인될 때까지 저장 오류를 반환합니다.
- 프록시끼리는 전용 advisory lock으로 갱신을 직렬화하고 저장 직전 원본을 비교합니다. 외부 Claude CLI는 이 잠금에 참여하지 않으므로 비교와 쓰기 사이의 경합은 남습니다. 원자적 CAS나 CLI의 비공개 잠금 규약과의 호환성을 보장하지 않습니다.

## 요청 변환

현재 구현은 Claude Code `2.1.282` 요청 프로필에 맞춰 동작하도록 고정되어 있습니다.

- `x-api-key`를 제거하고 OAuth Bearer 토큰을 `Authorization`에 넣습니다.
- Claude Code용 `x-app`, `anthropic-dangerous-direct-browser-access`, `anthropic-version`, User-Agent를 설정합니다.
- 유효한 `x-claude-code-session-id`가 없으면 세션 UUID를 생성합니다.
- native beta 목록을 병합하고, 캡처된 프로필에 없는 `context-1m`과 structured outputs beta는 제거합니다.
- `claude-opus-5-5` 요청에는 HTTP 빌링 헤더를 보내지 않습니다. 대신 본문 `system` 블록에 billing fingerprint와 Claude Agent SDK identity 블록을 추가합니다.
- 일반 요청의 XML 시스템 프롬프트 일부를 Markdown 형태로 바꿉니다.
- `todowrite`와 `background_output` 도구 이름을 각각 `task_write`와 `bg_output`으로 바꾸고, SSE 응답에서 원래 이름으로 되돌립니다.
- `/messages`의 `max_tokens`는 `16384`으로 제한합니다.
- upstream이 429를 반환하면 기본 15초 동안 새 요청을 잠시 중단합니다. upstream의 더 긴 `Retry-After` 값은 따릅니다.

### 벤치마크 단일 시도 모드

`X-CC-Proxy-Mode: benchmark-single-attempt-v1`을 보내면 프록시는 같은 응답 헤더로 지원 여부를 알리고, upstream 401에서 명시적 토큰 갱신 및 재시도를 하지 않습니다. `/health`에도 응답 헤더가 붙지만 기존 상태 코드와 JSON은 그대로이며, 이 헤더는 자격증명 상태나 추론 성공을 보증하지 않습니다. 알 수 없는 비어 있지 않은 모드 값은 upstream/자격증명 접근 전에 거부합니다. 내부 모드 헤더는 upstream에 전달되지 않고, 이 모드에서는 POST 재전송 표식이 되지 않도록 `Idempotency-Key`와 `X-Idempotency-Key`도 제거합니다. 헤더를 보내지 않은 일반 요청의 401 재시도와 본문 변환은 그대로입니다.

Opus 5.5 동작은 OpenCode를 통한 검증을 기준으로 합니다. OpenCode의 자동 제목 생성 요청은 다른 모델을 사용할 수 있으며, 제목 생성 실패나 거부 응답은 본문 요청과 별개의 동작입니다. 비대화형 검증에서는 지원되는 경우 명시적인 제목 요청을 사용하세요.

## 요구사항

- Go 1.25 이상
- macOS, Keychain 접근용
- Keychain 갱신 빌드는 cgo와 macOS SDK(Xcode Command Line Tools)가 필요합니다. cgo가 없는 빌드는 Keychain 쓰기 오류를 반환하며 파일 저장은 지원합니다.
- Claude Code 설치 및 OAuth 로그인
- OpenCode

## 빌드와 실행

```bash
go build -o cc-proxy .
./cc-proxy -listen 127.0.0.1:34156
```

기본 listen 주소는 `127.0.0.1:34156`입니다. 백그라운드로 시작하려면 터미널 세션의 로그와 프로세스를 직접 관리하세요.

```bash
nohup ./cc-proxy -listen 127.0.0.1:34156 >cc-proxy.log 2>&1 &
```

헬스 체크는 자격증명 상태까지 확인합니다.

```bash
curl http://127.0.0.1:34156/health
```

첫 실행 시 macOS Keychain 접근 대화상자가 나타나면 이 도구의 접근을 허용해야 합니다.

## OpenCode 설정

환경 변수로 임시 실행할 수 있습니다.

```bash
ANTHROPIC_BASE_URL=http://127.0.0.1:34156/v1 \
ANTHROPIC_API_KEY=proxy-placeholder \
opencode
```

`opencode.json`에서는 provider의 `baseURL`과 자리표시자 API 키를 설정하고, 모델을 `anthropic/claude-opus-5-5`로 선택합니다.

```json
{
  "provider": {
    "anthropic": {
      "options": {
        "baseURL": "http://127.0.0.1:34156/v1",
        "apiKey": "proxy-placeholder"
      }
    }
  },
  "model": "anthropic/claude-opus-5-5"
}
```

OpenCode 버전에 따라 설정 키가 달라질 수 있으므로 해당 버전의 설정 문서도 확인하세요.

## 테스트

```bash
go test ./...
go test -v ./...
```

## 보안과 사용 제한

- `127.0.0.1`에만 바인딩하고 포트를 외부에 노출하지 마세요.
- 자격증명, 로그 파일, 빌드 산출물을 저장소에 커밋하지 마세요.
- 로그에는 요청 본문과 응답 오류 본문을 기록하지 않고 상태, 요청 ID, 구조화된 SSE 결과만 기록합니다. 그래도 로그 파일은 민감할 수 있으니 보호하세요.
- 이 프로젝트는 제3자 클라이언트에서 구독 OAuth를 사용하는 비공식 방식입니다. Anthropic이 공식 지원하지 않으며, 약관과 호환성은 바뀔 수 있고 언제든 작동하지 않을 수 있습니다. 관련 제한은 [Claude Code 인증 및 자격증명 지침](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use)을 확인하세요.

## 버전 고정

Claude Code 요청 프로필과 프로토콜은 `2.1.282`에 고정되어 있습니다. Claude Code가 바뀌면 `CCVersion`, 요청 변환, 프로토콜 테스트를 함께 갱신하고 실제 OpenCode 경로를 다시 검증해야 합니다.

## 라이선스

MIT. 자세한 내용은 [`LICENSE`](LICENSE)를 참조하세요.
