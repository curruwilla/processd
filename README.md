# Processd

Process manager leve, em Go, que executa e supervisiona processos CLI através de uma REST API.

> **Run any CLI process through a simple API and keep it alive.**

Fica no espaço entre supervisores locais (Supervisor, PM2) e orquestradores distribuídos
(Nomad, Kubernetes): um binário, um arquivo de config, uma API.

---

## Status

**Alpha — MVP completo.** O daemon executa, supervisiona, persiste e recupera processos de verdade.
Ainda não foi usado em produção: trate a primeira instalação como piloto.

| Área | Estado |
|---|---|
| API REST, auth por token, contrato de erro, idempotência, paginação | pronto |
| Config do daemon e workers (YAML estrito), reload por SIGHUP | pronto |
| Validação de params e montagem de argv | pronto |
| Execução: process group, uid/gid, env limpo, sinais, timeout | pronto |
| Fila, limites global e por worker, TTL de fila | pronto |
| Locks persistidos (`queue` e `reject`) | pronto |
| Retry com backoff, jitter, `reset_after`, exit codes fatais | pronto |
| Captura de logs por tentativa, com limite de tamanho e leitura pela API | pronto |
| Persistência SQLite, histórico e trilha de auditoria | pronto |
| Crash recovery: fingerprint `(pid, starttime)`, `orphan_policy` | pronto |
| Graceful shutdown do daemon e da árvore de processos | pronto |
| Retenção e GC de histórico e de logs | pronto |
| Métricas Prometheus em `/v1/metrics`, com contadores e histograma por worker | pronto |
| CPU/memória por execução, amostradas de `/proc` | pronto |
| Streaming de logs por SSE, e `processd logs -f` | pronto |
| Console web embutido em `/ui/` | pronto |
| CLI completa | pronto |

Fora do MVP, por decisão: `type: service` e desired state, execução distribuída (Agents), TLS nativo,
tracing OpenTelemetry. Ver [docs/SPEC.md](docs/SPEC.md) §22 e §25.

## O que faz

* Executa processos CLI via REST API, com argumentos validados.
* Identifica cada execução por um ID lógico estável, independente do PID.
* Limita processos simultâneos (global e por worker) e enfileira o excedente.
* Captura stdout/stderr, exit code e sinal de término, e transmite a saída ao vivo.
* Expõe métricas Prometheus, uso de CPU/memória por execução e um console web embutido.
* Aplica timeout, retry com backoff e locks contra execução concorrente.
* Persiste estado em SQLite e reconcilia após restart.
* Faz graceful shutdown do daemon e da árvore de processos.

## O que não faz

Orquestração de containers, service mesh, consenso distribuído, auto-scaling, provisionamento.
Não substitui o systemd para serviços de sistema.

---

## Início rápido

### 1. Instalar

Baixe o binário da release mais recente (Linux `amd64` ou `arm64`):

```bash
VERSION=0.1.0                               # ver github.com/curruwilla/processd/releases
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.tar.gz"
tar -xzf "processd_${VERSION}_linux_${ARCH}.tar.gz" processd
sudo install -m 0755 processd /usr/local/bin/processd
```

Cada release publica também `checksums.txt`, para conferir o download:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
```

Ou instale o pacote da sua distro (`.deb`, `.rpm`, `.apk`, `amd64` e `arm64`):

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.deb"
sudo dpkg -i "processd_${VERSION}_linux_${ARCH}.deb"    # ou: sudo rpm -i ...rpm
```

O pacote instala só o binário em `/usr/bin/processd` e os exemplos em
`/usr/share/doc/processd/examples/`. Config, token e unit continuam sendo do `processd setup`, que
aponta a unit para o binário que o executou.

Ou compile a partir do código:

```bash
git clone https://github.com/curruwilla/processd && cd processd
make build                                  # bin/processd, build sem CGO
sudo install -m 0755 bin/processd /usr/local/bin/processd
```

<details>
<summary>Container</summary>

```bash
docker run -d --name processd \
  -p 7373:7373 \
  -v processd-etc:/etc/processd \
  -v processd-data:/var/lib/processd \
  -v processd-logs:/var/log/processd \
  ghcr.io/curruwilla/processd:${VERSION}
```

No primeiro start o entrypoint roda `processd setup` dentro do container: escreve
`/etc/processd/processd.yaml`, gera o token e imprime tudo no log (`docker logs processd`). O bind
padrão vira `0.0.0.0:7373`; mude com `-e PROCESSD_LISTEN=...`. Os três volumes preservam token,
workers, banco e logs entre restarts.

A imagem é Alpine, não `scratch`: o daemon supervisiona processos CLI, então precisa de um userland
onde os comandos dos workers existam. Instale no container o que seus workers chamam.

</details>

<details>
<summary>Verificar assinatura e SBOM</summary>

O `checksums.txt` de cada release é assinado com [cosign](https://docs.sigstore.dev/) em modo
keyless: a assinatura fica atrelada à identidade do workflow que publicou a release, não a uma
chave privada. Verificar exige cosign v2+:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt"
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt.sigstore.json"

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/curruwilla/processd/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt
```

Com o `checksums.txt` verificado, o `sha256sum --check` acima já garante os binários e pacotes.

A imagem do container também é assinada:

```bash
cosign verify ghcr.io/curruwilla/processd:${VERSION} \
  --certificate-identity "https://github.com/curruwilla/processd/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
```

Cada arquivo `.tar.gz`, `.deb`, `.rpm` e `.apk` vem com um SBOM SPDX ao lado
(`<artefato>.spdx.json`): a lista de todos os módulos Go que entraram no binário, com versão. Serve
para responder "essa instalação é afetada por esse CVE?" sem recompilar:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.tar.gz.spdx.json"
jq -r '.packages[] | "\(.name) \(.versionInfo)"' "processd_${VERSION}_linux_${ARCH}.tar.gz.spdx.json"
```

</details>

### 2. Preparar o node

```bash
sudo processd setup
```

Um comando entrega o node inteiro: cria `/etc/processd/workers.d`, `/var/lib/processd` e
`/var/log/processd`, escreve `/etc/processd/processd.yaml`, gera um token de API, instala
`/etc/systemd/system/processd.service` e sobe o serviço. No fim imprime o token e todos os
caminhos e endereços do node — config, logs, dados, unit, API, console web, métricas — junto dos
comandos para conferir cada um deles.

O daemon guarda só o digest do token, então o segredo em texto puro fica em `/etc/processd/token`
(modo `0600`, dono root). Esse arquivo é o que evita o `unauthorized` clássico depois do setup:

* os comandos cliente (`processd ps`, `run`, `logs`, ...) leem o arquivo quando `--token` e
  `PROCESSD_TOKEN` não estão definidos — como root, não é preciso colar token nenhum;
* rodar `processd setup` de novo reaproveita o token já instalado, em vez de invalidar quem já
  está configurado. Para trocar de propósito: `sudo processd setup --rotate-token`.

Outras flags: `--dry-run` mostra o que faria sem tocar em nada, `--listen` muda o bind,
`--systemd=false` pula o unit, `--start=false` instala e habilita sem iniciar, `--output json`
devolve o mesmo relatório para script.

A config é reescrita a partir dos valores que o daemon leu — o que descarta comentários e reordena
chaves — e o arquivo anterior fica salvo ao lado como `processd.yaml.bak-<timestamp>`.

<details>
<summary>Setup manual, sem <code>processd setup</code></summary>

```bash
sudo mkdir -p /etc/processd/workers.d /var/lib/processd /var/log/processd
TOKEN=$(openssl rand -hex 32)
printf '%s' "$TOKEN" | processd token hash   # imprime sha256:...
```

Só o hash vai para o arquivo; o segredo nunca é gravado em disco pelo daemon. O token é lido de
stdin justamente para não aparecer na lista de processos nem no histórico do shell.

```yaml
# /etc/processd/processd.yaml
listen: 127.0.0.1:7373
data_dir: /var/lib/processd
log_dir: /var/log/processd
workers_dir: /etc/processd/workers.d

max_processes: 50

auth:
  tokens:
    - name: dev
      hash: "sha256:<cole o hash aqui>"
```

</details>

### 3. Declarar um worker

```yaml
# /etc/processd/workers.d/backup.yaml
version: 1
workers:
  - name: backup
    command: /usr/bin/rsync
    args: ["-a", "/data/{{bucket}}/", "/backup/{{bucket}}/"]
    params:
      bucket: { required: true, pattern: "^[a-z0-9-]{1,32}$" }
    cwd: /
    user: backup
    timeout: 1h
    max_processes: 2
    lock: "backup:{{bucket}}"
    retry:
      enabled: true
      max_attempts: 3
      backoff: { type: exponential, initial: 10s, max: 2m, jitter: 0.2 }
```

Referência completa dos campos em [Definição de workers](#definição-de-workers). A carga é
tudo-ou-nada: um arquivo inválido derruba o reload inteiro, então um worker nunca desaparece em
silêncio de um daemon vivo.

### 4. Subir o daemon

`processd setup` já deixou o serviço rodando; para conferir:

```bash
systemctl status processd
journalctl -u processd -f
```

Em primeiro plano, sem systemd:

```bash
processd serve --config /etc/processd/processd.yaml
```

O unit gerado sai de [`examples/processd.service`](examples/processd.service), com
`TimeoutStopSec` calculado a partir de `shutdown_grace` (+15s de folga): se o timeout do systemd
vencer antes, o daemon morre no meio do encerramento gracioso.

### 5. Disparar e acompanhar

Como root no próprio node, o token sai de `/etc/processd/token` sozinho. De outra máquina, ou
como outro usuário, aponte o cliente:

```bash
export PROCESSD_SERVER=http://127.0.0.1:7373
export PROCESSD_TOKEN=$(sudo cat /etc/processd/token)

processd run backup --param bucket=faturas    # devolve o id da execução
processd ps --status RUNNING
processd logs -f proc_01KABCDEF...            # acompanha a saída ao vivo
processd stop proc_01KABCDEF... --grace 15s
```

O mesmo pela API:

```bash
curl -X POST http://127.0.0.1:7373/v1/processes \
  -H "Authorization: Bearer $PROCESSD_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"worker":"backup","params":{"bucket":"faturas"}}'
```

```json
{ "id": "proc_01KABCDEF...", "status": "RUNNING", "pid": 18231, "attempt": 1 }
```

Console web em `http://127.0.0.1:7373/ui/`: painel do nó, execuções com filtro, detalhe com
CPU/memória ao vivo, logs em streaming e disparo de workers. A página é estática e servida sem
token — pede o token ao operador e usa a mesma API pública. Desligue com `ui: { enabled: false }`.

Métricas em `/v1/metrics`, no formato texto do Prometheus.

### Depois de editar um worker

```bash
processd reload                  # ou: sudo systemctl kill -s HUP processd
processd workers                 # confere o que o daemon carregou
```

Execuções em andamento mantêm a definição com que nasceram: o reload nunca muda um processo que já
está rodando.

---

## CLI

Um único binário, cliente da mesma API pública. Configuração do cliente por `--server`/
`PROCESSD_SERVER` e `--token`/`PROCESSD_TOKEN`; sem token explícito, o cliente lê o arquivo
gravado por `processd setup` ao lado da config (`/etc/processd/token`).

| Comando | O que faz |
|---|---|
| `processd setup [--dry-run] [--rotate-token] [--systemd=false] [--start=false]` | instala o node: diretórios, config, token, unit systemd, e imprime tudo |
| `processd serve --config <path>` | sobe o daemon |
| `processd status` | saúde, versão, slots, execuções rodando e na fila |
| `processd ps [--status S] [--worker w] [--limit n] [--cursor c] [--output table\|json]` | lista execuções |
| `processd run <worker> [--param nome=valor] [--lock k]` | cria uma execução |
| `processd logs <id> [--stream stdout\|stderr\|both] [--attempt n] [--tail n] [-f]` | saída capturada, com `-f` em streaming |
| `processd stop <id> [--grace 15s]` | SIGTERM no grupo, SIGKILL depois da graça |
| `processd signal <id> <SINAL>` | envia um sinal do allowlist ao grupo |
| `processd workers` | workers carregados, com os params declarados |
| `processd reload` | recarrega `workers.d` |
| `processd token hash` | lê um token de stdin e imprime o digest da config |

Sinais aceitos: `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2`, `SIGKILL`,
`SIGSTOP`, `SIGCONT`. Qualquer outro é recusado com `400`, e todo sinal atinge o **process group**
inteiro — sinalizar só o PID deixaria netos vivos.

---

## Definição de workers

Arquivos `*.yaml` em `workers_dir` (default `/etc/processd/workers.d`), cada um com `version: 1` e
uma lista `workers`. O nome é chave global do daemon, não do arquivo. A decodificação é **estrita**:
chave desconhecida é erro de carga, nunca um default silencioso.

Durações aceitam a sintaxe do Go mais `d` e `w`: `30s`, `5m`, `1h30m`, `30d`, `2w`. Tamanhos aceitam
sufixo IEC ou SI: `32MiB`, `32MB`, ou bytes puros.

### Campos do worker

| Chave | Tipo | Default | Valores e regras |
|---|---|---|---|
| `name` | string | — obrigatório | único entre todos os arquivos |
| `enabled` | bool | `true` | `false` faz o daemon carregar o worker e recusar execuções com `422` |
| `type` | enum | `task` | `task` é o único aceito hoje; `service` é reservado para a fase 4 |
| `command` | string | — obrigatório | caminho **absoluto**, executado direto — nunca por shell |
| `args` | lista de strings | `[]` | pode conter `{{param}}`; a substituição não divide elementos |
| `params` | mapa | `{}` | declaração dos valores que o request pode enviar (tabela abaixo) |
| `cwd` | string | `/` | caminho absoluto; precisa existir e ser diretório |
| `user` | string | vazio | **nome** de usuário do sistema, não uid. Vazio com daemon como root → start recusado, a menos que `allow_root_processes: true` |
| `group` | string | grupo primário do `user` | nome de grupo do sistema; grupos suplementares são aplicados |
| `env` | mapa string→string | `{}` | ambiente do filho. O ambiente do daemon **não** é herdado: ele guarda segredos |
| `env_passthrough` | lista de strings | `[]` | nomes de variáveis do daemon repassadas explicitamente, ex.: `[PATH, LANG, TZ]` |
| `timeout` | duração | `0` = sem timeout | ao estourar: `SIGTERM` no grupo → `kill_grace` → `SIGKILL` |
| `kill_grace` | duração | `15s` | espera entre o `SIGTERM` e o `SIGKILL` |
| `max_processes` | int ≥ 0 | `0` = só o limite global | teto de simultâneas do worker; o excedente **espera na fila**, não é recusado |
| `lock` | string | vazio = sem lock | chave de exclusão mútua, pode conter `{{param}}` |
| `lock_conflict` | enum | `queue` | `queue` espera o lock liberar; `reject` responde `409` na hora |
| `overridable` | lista enum | `[]` | o que o request pode sobrescrever: `env`, `timeout`, `lock`. Override não listado → `400` |
| `retry` | objeto | desligado | tabela abaixo |
| `logs.max_bytes_per_stream` | tamanho | o valor do daemon (`32MiB`) | cap por stream por tentativa; a retenção é do daemon, não do worker |

### `params`

Argumentos chegam ao processo **apenas** por params declarados e validados.

| Chave | Tipo | Default | Regra |
|---|---|---|---|
| `required` | bool | `false` | ausente no request → `400` |
| `pattern` | regex RE2 | vazio | compilada na carga do worker; valor fora do padrão → `400` |
| `enum` | lista de strings | `[]` | valor fora da lista → `400` |
| `default` | string | vazio | usado quando o param é opcional e não veio no request |

Substituição (`docs/SPEC.md` §5.3):

1. `{{nome}}` só é resolvido **dentro** de elementos de `args` e em `lock`.
2. Nunca cria, divide ou junta elementos de argv: um valor com espaços continua sendo um argumento.
3. Placeholder não declarado em `params` **impede a carga** do worker — um typo não vira `{{id}}`
   literal na linha de comando.
4. Param enviado e não declarado → `400`.
5. Elemento de argv que é só um placeholder opcional ausente é removido, em vez de virar `""`.

### `retry`

Os defaults abaixo só são aplicados quando `enabled: true`.

| Chave | Tipo | Default | Valores |
|---|---|---|---|
| `enabled` | bool | `false` | sem isso, uma tentativa e pronto |
| `max_attempts` | int ≥ 1 | `1` | total, incluindo a primeira tentativa |
| `retry_on` | lista enum | `[nonzero_exit, signal, start_error]` | `nonzero_exit`, `signal`, `start_error`, `timeout` |
| `success_exit_codes` | lista de int | `[0]` | código listado → `COMPLETED` |
| `no_retry_exit_codes` | lista de int | `[]` | código listado → `FAILED` imediato, sem retry |
| `reset_after` | duração | `0` = desligado | tentativa que durou mais que isso zera o contador |
| `on_shutdown` | bool | `false` | `true` devolve a execução à fila no shutdown, em vez de cancelá-la |
| `backoff.type` | enum | `exponential` | `exponential`, `linear`, `fixed` |
| `backoff.initial` | duração | `5s` | atraso da primeira repetição |
| `backoff.max` | duração | `5m` | teto do atraso |
| `backoff.multiplier` | float > 0 | `2` | usado só por `exponential` |
| `backoff.jitter` | float 0..1 | `0` | espalha o atraso em `±jitter`; sem ele, tudo que falhou junto repete junto |

Curvas: `fixed` = `initial`; `linear` = `initial × tentativa`; `exponential` =
`initial × multiplier^(tentativa-1)`. O teto `max` é aplicado antes do jitter.

O lock é mantido entre tentativas: soltá-lo durante o backoff deixaria outra execução tomá-lo no meio
do retry.

### Configuração do daemon

| Chave | Default | Valores e regras |
|---|---|---|
| `listen` | `127.0.0.1:7373` | endereço do HTTP; sem TLS nativo, ponha um proxy na frente para expor |
| `data_dir` | `/var/lib/processd` | SQLite do estado |
| `log_dir` | `/var/log/processd` | arquivos de saída por tentativa |
| `workers_dir` | `/etc/processd/workers.d` | onde os `*.yaml` de worker são lidos |
| `max_processes` | `50` | teto do nó inteiro |
| `shutdown_grace` | `30s` | orçamento dos processos no encerramento |
| `orphan_policy` | `kill` | `kill` mata o processo que sobreviveu ao daemon antes do retry; `leave` mantém e não repete |
| `execution_mode` | `workers` | `workers` só executa workers; `raw` aceita comando do cliente e exige `allowed_commands` — comando livre é RCE por design |
| `allowed_commands` | `[]` | allowlist de caminhos absolutos, usada só no modo `raw` |
| `allow_root_processes` | `false` | permite rodar sem `user` quando o daemon é root |
| `queue.max_depth` | `1000` | fila cheia → `429` |
| `queue.item_ttl` | `1h` | item que espera mais que isso vira `queue_timeout` |
| `history.retention` | `30d` | GC do histórico de execuções |
| `history.max_rows` | `500000` | teto de linhas retidas |
| `logs.max_bytes_per_stream` | `32MiB` | cap por stream por tentativa; atingido, marca `log_truncated` |
| `logs.retention` | `14d` | GC dos arquivos de log |
| `ui.enabled` | `true` | console web em `/ui/` |
| `auth.tokens[].name` | — | identifica o token na trilha de auditoria |
| `auth.tokens[].hash` | — | `sha256:...`, gerado por `processd setup` ou `processd token hash`; o segredo correspondente fica em `/etc/processd/token` |
| `auth.tokens[].workers` | `[]` = todos | restringe o token a workers específicos |

Exemplos completos em [`examples/`](examples/); a especificação de cada campo, com o porquê, em
[`docs/SPEC.md`](docs/SPEC.md) §5 e §20.

---

## Desenvolvimento

Requer Go 1.25+ e Linux.

```bash
make install-tools     # golangci-lint e govulncheck
make build             # bin/processd
make test-race         # testes com detector de corrida
make test-integration  # testes ponta a ponta (sobe daemons e processos reais)
make lint              # golangci-lint
make fmt               # formatação
make release-check     # valida .goreleaser.yml
make release-snapshot  # gera archives, pacotes e SBOMs em dist/, sem publicar
make release-docker    # constrói a imagem do container localmente
```

Releases são automáticas: um push de tag `v*` dispara
[`.github/workflows/release.yml`](.github/workflows/release.yml), que roda os testes, o GoReleaser
e publica na release do GitHub os `.tar.gz`, os pacotes `.deb`/`.rpm`/`.apk`, um SBOM SPDX por
artefato, o `checksums.txt` e a assinatura cosign dele. No mesmo passo sobem as imagens
`ghcr.io/curruwilla/processd` (`amd64` e `arm64`), também assinadas.

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

Layout:

```
cmd/processd/        entrypoint
internal/cli/        árvore de comandos cobra, cliente da própria API
internal/daemon/     montagem do grafo de dependências e ciclo de vida
internal/api/        handlers HTTP, auth, contrato de erro
internal/core/       domínio e máquina de estados, sem I/O
internal/config/     config do daemon e definição de workers
internal/queue/      admissão, slots, backoff
internal/supervisor/ supervisão de cada execução
internal/runner/     exec, process groups, sinais, /proc
internal/logstore/   arquivos de log por tentativa, leitura e streaming
internal/metrics/    contadores e histogramas no formato texto do Prometheus
internal/webui/      console web embutido (go:embed)
internal/store/      interface de persistência + SQLite
```

Exemplos de configuração e unit systemd em [`examples/`](examples/).

---

## Princípios

* **Simple by default** — sem Redis, Kafka, Docker ou Kubernetes.
* **API-first** — a CLI é cliente da mesma API pública.
* **Single binary** — instalar é copiar um arquivo.
* **Process-first** — a abstração é processo/execução, não container.
* **Fail closed** — na dúvida, recusa: sem token, sem `user` definido rodando como root, ou com
  parâmetro fora do padrão declarado, nada executa.
* **Linux-first** — depende de process groups POSIX, sinais e `/proc`.

---

## Segurança

Execução usa `exec.Command(cmd, args...)`, nunca `sh -c`. Por padrão o daemon roda em
`execution_mode: workers`: só executa workers pré-configurados, e argumentos chegam apenas por
parâmetros declarados e validados por regex/enum. Autenticação por token é obrigatória, o bind
default é `127.0.0.1` e processos não rodam como root sem configuração explícita.

Detalhes em [docs/SPEC.md §16](docs/SPEC.md).

---

## Roadmap

| Fase | Entrega |
|---|---|
| 1 ✅ | Process manager local: API, estados, persistência, auth |
| 2 ✅ | Supervisor: fila, locks, retry/backoff, timeout, recovery |
| 3 ✅ | Observabilidade: métricas, streaming de logs, CPU/memória, Web UI |
| 4 | `type: service` e desired state local |
| 5 | Agents e execução distribuída |

---

## Licença

[Apache 2.0](LICENSE)
