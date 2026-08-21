# Processd

*Also available in [English](README.md).*

> **Rode qualquer processo CLI por uma API simples e mantenha ele vivo.**

Process manager leve, escrito em Go. Executa e supervisiona processos CLI através de uma REST API:
um binário, um arquivo de configuração, uma API. Fica no espaço entre supervisores locais
(Supervisor, PM2) e orquestradores distribuídos (Nomad, Kubernetes).

**Status: alpha.** O MVP está completo — o daemon executa, supervisiona, persiste e recupera
processos de verdade — mas ainda não foi usado em produção. Trate a primeira instalação como piloto.

## O que faz

* Executa processos CLI por uma REST API, com argumentos validados e sem shell no caminho.
* Dois tipos de execução: uma `task` roda uma vez e seu sucesso é final; um `service` não deveria
  terminar, e qualquer saída faz ele reiniciar.
* Identifica cada execução por um ID lógico estável, que sobrevive a retries e a restarts do daemon.
* Limita a concorrência global e por worker, e enfileira o excedente.
* Captura stdout e stderr por tentativa, junto do exit code e do sinal de término, e transmite a
  saída ao vivo.
* Aplica timeout, retry com backoff e locks contra execução concorrente.
* Persiste o estado em SQLite e reconcilia o que encontra depois de um restart.
* Expõe métricas Prometheus, CPU e memória por execução, e um console web embutido.
* Encerra o daemon e toda a árvore de processos de forma graciosa.

## O que não faz

Orquestração de containers, service mesh, consenso distribuído, auto-scaling, provisionamento,
desired state e réplicas, TLS nativo, tracing OpenTelemetry. Não substitui o systemd para serviços
de sistema.

Ele supervisiona processos **no host onde roda**, usando os runtimes, os diretórios de projeto e as
contas de usuário que já estão lá.

---

## Início rápido

### 1. Instalar o binário

```bash
VERSION=0.1.0                               # github.com/curruwilla/processd/releases
ARCH=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.tar.gz"
tar -xzf "processd_${VERSION}_linux_${ARCH}.tar.gz" processd
sudo install -m 0755 processd /usr/local/bin/processd
```

Toda release também publica o `checksums.txt`:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
```

<details>
<summary>Pacote da distro, ou compilar do código</summary>

`.deb`, `.rpm` e `.apk`, para `amd64` e `arm64`:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/processd_${VERSION}_linux_${ARCH}.deb"
sudo dpkg -i "processd_${VERSION}_linux_${ARCH}.deb"        # ou: sudo rpm -i ...rpm
```

O pacote instala o binário em `/usr/bin/processd` e os exemplos em
`/usr/share/doc/processd/examples/`. Configuração, token e unit systemd continuam sendo do
`processd setup`.

Do código (Go 1.25+, Linux):

```bash
git clone https://github.com/curruwilla/processd && cd processd
make build                                  # bin/processd, sem CGO
sudo install -m 0755 bin/processd /usr/local/bin/processd
```

Instale o binário em um caminho de sistema — `/usr/local/bin` ou `/usr/bin`. O unit systemd aponta
para o binário que executou o `processd setup`, então um unit mirando uma árvore de build quebra no
momento em que essa árvore é recompilada ou movida.

</details>

### 2. Preparar o node

```bash
sudo processd setup
```

Um comando entrega o node inteiro:

* cria `/etc/processd/workers.d`, `/var/lib/processd` e `/var/log/processd`;
* escreve `/etc/processd/processd.yaml`;
* gera um token de API, guardado como digest na configuração e em texto puro em
  `/etc/processd/token` (modo `0600`, dono root);
* instala `/etc/systemd/system/processd.service`, apontando para o binário que o executou, e sobe o
  serviço;
* imprime o token e todos os caminhos, endereços e comandos de verificação.

Os comandos cliente leem `/etc/processd/token` quando nem `--token` nem `PROCESSD_TOKEN` estão
definidos — como root no node você nunca cola o segredo. Rodar o setup de novo reaproveita o token
instalado, em vez de invalidar todo cliente já configurado; `--rotate-token` troca de propósito.

Outras flags: `--dry-run` relata o que faria sem tocar em nada, `--listen` muda o bind,
`--systemd=false` pula o unit, `--start=false` instala e habilita sem iniciar, `--output json`
devolve o mesmo relatório para script.

A configuração é reescrita a partir dos valores que o daemon leu — comentários se perdem, chaves são
reordenadas — e o arquivo anterior fica salvo ao lado como `processd.yaml.bak-<timestamp>`.

Conferir:

```bash
systemctl status processd
processd status
journalctl -u processd -f
```

### 3. Declarar um worker

Um worker é um comando que a API tem permissão de executar. Um worker por arquivo mantém um reload
que falhou apontando para o lugar certo:

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

```bash
sudo processd reload      # relê workers.d, imprime "N workers loaded"
processd workers          # confirma o que o daemon carregou
```

### 4. Executar

```bash
processd run backup --param bucket=faturas     # devolve o id da execução
processd ps
processd logs -f proc_01KABCDEF...
processd stop proc_01KABCDEF... --grace 15s
```

O mesmo pela API:

```bash
curl -X POST http://127.0.0.1:7373/v1/processes \
  -H "Authorization: Bearer $(sudo cat /etc/processd/token)" \
  -H "Content-Type: application/json" \
  -d '{"worker":"backup","params":{"bucket":"faturas"}}'
```

```json
{ "id": "proc_01KABCDEF...", "status": "STARTING", "pid": null, "attempt": 1 }
```

De outra máquina, ou como outro usuário, aponte o cliente para o node:

```bash
export PROCESSD_SERVER=http://127.0.0.1:7373
export PROCESSD_TOKEN=...
```

O console web fica em <http://127.0.0.1:7373/ui/>: painel do node, execuções com filtro, CPU e
memória ao vivo, logs em streaming e um formulário para disparar workers. É uma página estática
servida sem token — ela pede o token ao operador e chama a mesma API. Desligue com
`ui: { enabled: false }`. As métricas Prometheus ficam em `/v1/metrics`.

---

## Operando um node

### Colinha de comandos

| Quero | Comando |
|---|---|
| ver saúde e slots livres | `processd status` |
| ver o que está rodando | `processd ps`, `processd ps --type service`, `processd ps --status FAILED` |
| carregar os arquivos de worker depois de editar | `sudo processd reload` |
| ver o que o daemon carregou | `processd workers` |
| rodar uma task | `processd run <worker> --param nome=valor` |
| subir um service | `processd run <worker>` |
| ler a saída | `processd logs <id>`, com `-f` para acompanhar |
| parar alguma coisa | `processd stop <id> [--grace 15s]` |
| reiniciar com a definição atual | `processd restart <id>` |
| cutucar um processo rodando | `processd signal <id> SIGHUP` |

### Adicionar um worker ou um service

1. **Escreva o arquivo** em `/etc/processd/workers.d/`. O loader valida a forma da definição, não o
   host onde ela vai rodar, então confira você mesmo — senão o reload passa e a primeira execução
   falha:
   * `command` existe neste host (o loader só verifica que o caminho é absoluto);
   * `cwd` existe;
   * o `user` existe — `id www-data`.

   Duas coisas o loader pega: um `name` já usado por outro arquivo, e um nome de arquivo que não
   termina em `.yaml` — um `.yml` não é erro, simplesmente nunca é lido.

2. **Carregue:**

   ```bash
   sudo processd reload
   processd workers
   ```

   A carga é tudo-ou-nada: um worker inválido em qualquer arquivo derruba o reload inteiro, o daemon
   fica com o conjunto anterior, e o erro nomeia o arquivo. Uma edição ruim nunca faz um worker
   sumir em silêncio de um node vivo.

3. **Suba.** O reload só registra definições; ele nunca inicia nada.
   * Uma **task** roda quando você pede: `processd run backup --param bucket=faturas`.
   * Um **service** precisa de um `processd run <worker>` para criar a execução que então se mantém
     viva. Confira com `processd ps --type service`.

Um service toma seu slot na admissão ou é recusado com `no_capacity` — ele nunca é enfileirado. Olhe
o `processd status` para ver slots livres antes de acrescentar um.

### Alterar um worker ou um service

Edite o arquivo, depois recarregue:

```bash
sudo processd reload
```

**O reload nunca muda um processo que já está rodando.** Toda execução mantém a definição com que
nasceu, então o próximo passo depende do que mudou:

| Situação | O que fazer |
|---|---|
| worker de task | nada — o próximo `processd run` usa a definição nova |
| service rodando | `processd restart <id>`: para a execução e cria outra, com id novo, a partir da definição recém-carregada |
| só a configuração do próprio programa mudou | `processd signal <id> SIGHUP`, se ele recarrega no `SIGHUP` — nada reinicia e o id continua o mesmo |
| worker renomeado | o nome novo é registrado; o service antigo segue rodando com a definição antiga. `processd stop <id antigo>`, depois `processd run <nome novo>` |
| worker desabilitado (`enabled: false`) | novas execuções são recusadas com `422`; o que já roda fica intacto. Pare com `processd stop <id>` |
| arquivo do worker apagado | igual ao desabilitado, e pior: quando a tentativa em curso terminar não sobra política nenhuma para trazer o processo de volta. Pare explicitamente |

O `processd restart` confere o worker antes de parar qualquer coisa, e recusa quando ele sumiu ou
está desabilitado — do contrário você ficaria com um service parado e nada para subir de novo.

### Tocar services

| Ação | Comando | O que acontece |
|---|---|---|
| subir | `processd run api` | uma execução, supervisionada, reiniciada a cada saída |
| parar de vez | `processd stop <id>` | `SIGTERM` no process group, `SIGKILL` depois do `kill_grace`. Termina em `CANCELED` com `reason: user_request` e **nunca** dispara retry |
| reiniciar | `processd restart <id>` | para, espera o slot, cria uma execução nova com a definição atual |
| recarregar a config dele | `processd signal <id> SIGHUP` | o sinal atinge o process group inteiro; a execução continua a mesma |
| acompanhar | `processd ps --type service` | a coluna `RESTARTS` mostra o quanto o node está brigando para manter ele de pé |

Uma parada deliberada é o único jeito de um service terminar sem voltar. Qualquer outra saída —
limpa, com erro, ou morto por sinal — é um restart, a menos que o exit code esteja em
`no_retry_exit_codes`.

### Atualizar o processd

`processd setup` **não** é o comando de update: `systemctl enable --now` não reinicia um serviço que
já está rodando, então o binário novo ficaria no disco sem uso. O setup é para a primeira instalação
e para quando o endereço de bind, o caminho do unit ou o caminho do binário mudaram.

```bash
# 1. saiba qual binário o unit realmente inicia
systemctl cat processd | grep ExecStart

# 2. faça backup do estado — migration é via de mão única
sudo systemctl stop processd
sudo cp -a /var/lib/processd /var/lib/processd.bak-$(date +%F)

# 3. troque esse binário
sudo install -m 0755 ./processd /usr/local/bin/processd

# 4. suba e confira
sudo systemctl start processd
processd status                 # versão, slots, rodando, na fila
processd ps --type service      # tudo de volta?
```

* **Instale CLI e daemon do mesmo build.** É o mesmo binário; um cliente mais velho que o daemon
  simplesmente não tem os comandos novos, o que é confuso de debugar.
* **As migrations rodam sozinhas** no start, uma vez cada, em ordem de nome de arquivo. Não existe
  caminho de downgrade depois que uma rodou — é para isso que serve o passo 2.
* **Services com `retry.on_shutdown: true`** voltam para a fila no shutdown e sobem sozinhos no
  próximo start, mantendo o id. Sem isso terminam em `CANCELED` e precisam de `processd run`.
* **Tasks em voo** têm o `shutdown_grace` (default `30s`) para terminar; o que sobrar é morto. O
  unit gerado põe `TimeoutStopSec` nesse valor mais 15s, então o systemd nunca corta o daemon no
  meio de um encerramento gracioso.

Editar workers não precisa de nada disso — isso é `processd reload`.

### Backup e retenção

| O quê | Onde | Importa porque |
|---|---|---|
| estado | `/var/lib/processd` (SQLite) | execuções, histórico, locks, chaves de idempotência |
| configuração e token | `/etc/processd` | todo cliente configurado autentica contra isso |
| saída | `/var/log/processd` | stdout e stderr por tentativa |

Faça o backup do estado com o daemon parado, para o arquivo do banco ficar consistente — e sempre
antes de um upgrade. A retenção é aplicada pelo próprio daemon: `history.retention` e
`history.max_rows` para execuções, `logs.retention` para os arquivos de saída.

### Monitorar

| Sinal | Como |
|---|---|
| liveness | `GET /v1/health` — público, sem token. `?deep=1` também pinga o store e responde `503` quando ele está inacessível |
| resumo do node | `processd status`, ou `GET /v1/stats` para os mesmos números em JSON |
| Prometheus | scrape em `GET /v1/metrics` (formato texto, exige token) |
| log do daemon | `journalctl -u processd`, linhas JSON do `log/slog` |
| saída da execução | `processd logs <id>`, `-f` para acompanhar uma tentativa viva |

Um `service` saudável não produz estado terminal nenhum, então os contadores comuns ficam em
silêncio sobre ele. Alerte em `processd_service_restarts_total` — um service em loop de restart é
invisível em todas as outras famílias.

### Proteger o node

* **Não existe TLS nativo.** O bind default é `127.0.0.1`; ponha um reverse proxy (nginx, Caddy,
  Traefik) na frente para qualquer acesso remoto.
* **Toda requisição exige token**, exceto `GET /v1/health`. Dê um para cada cliente, restrito aos
  workers que ele pode executar:

  ```yaml
  auth:
    tokens:
      - name: billing-cron
        hash: "sha256:..."      # printf '%s' "$TOKEN" | processd token hash
        workers: ["invoice-process"]
      - name: ops
        hash: "sha256:..."      # sem a chave workers: todos os workers
  ```

  O nome do token é o que aparece na trilha de auditoria. Para rotacionar:
  `sudo processd setup --rotate-token`, depois `sudo systemctl restart processd` — o daemon lê a
  autenticação só no start.
* **Mantenha `execution_mode: workers`** (o default). O modo `raw` deixa o cliente escolher o
  comando e é remote code execution por design; ele ainda exige uma allowlist `allowed_commands`
  explícita.
* **Mantenha `allow_root_processes: false`.** Com isso desligado, um worker sem `user` é recusado em
  vez de rodar como root em silêncio.

### Runbook

| Tarefa | Comando |
|---|---|
| adicionar um worker | escreva `workers.d/<nome>.yaml`, `sudo processd reload`, `processd workers` |
| subir um service novo | `processd run <worker>` |
| publicar mudança de worker | edite o arquivo, `sudo processd reload` |
| aplicar essa mudança a um service rodando | `processd restart <id>` |
| parar um service de vez | `processd stop <id>` |
| investigar uma falha | `processd ps --status FAILED`, depois `processd logs <id>` |
| rotacionar o token da API | `sudo processd setup --rotate-token`, `sudo systemctl restart processd` |
| atualizar o processd | backup de `/var/lib/processd`, troque o binário, `sudo systemctl restart processd` |
| reiniciar o node inteiro | `sudo systemctl restart processd` — services com `on_shutdown: true` voltam sozinhos |

---

<details>
<summary>Configurar um node na mão, sem <code>processd setup</code></summary>

```bash
sudo mkdir -p /etc/processd/workers.d /var/lib/processd /var/log/processd
TOKEN=$(openssl rand -hex 32)
printf '%s' "$TOKEN" | processd token hash   # imprime sha256:...
```

Só o digest vai para o arquivo. O token é lido de stdin justamente para não aparecer na lista de
processos nem no histórico do shell.

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
      hash: "sha256:<cole o digest aqui>"
```

Rode em primeiro plano com `processd serve --config /etc/processd/processd.yaml`, ou copie
[`examples/processd.service`](examples/processd.service) para `/etc/systemd/system/`. Qualquer que
seja o unit, o `TimeoutStopSec` precisa ser maior que o `shutdown_grace`, senão o systemd mata o
daemon no meio da parada dos process groups que ele supervisiona.

</details>

<details>
<summary>Verificar assinaturas e SBOMs</summary>

O `checksums.txt` é assinado com [cosign](https://docs.sigstore.dev/) em modo keyless: a assinatura
fica atrelada à identidade do workflow que publicou a release, não a uma chave privada. Verificar
exige cosign v2+:

```bash
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt"
curl -fsSLO "https://github.com/curruwilla/processd/releases/download/v${VERSION}/checksums.txt.sigstore.json"

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/curruwilla/processd/.github/workflows/release.yml@refs/tags/v${VERSION}" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  checksums.txt
```

Com o `checksums.txt` verificado, o `sha256sum --check` acima cobre todo binário e pacote.

Cada `.tar.gz`, `.deb`, `.rpm` e `.apk` vem com um SBOM SPDX ao lado (`<artefato>.spdx.json`): todo
módulo Go que entrou no binário, com versão, para responder "essa instalação é afetada por esse
CVE?" sem recompilar nada.

```bash
jq -r '.packages[] | "\(.name) \(.versionInfo)"' "processd_${VERSION}_linux_${ARCH}.tar.gz.spdx.json"
```

</details>

---

## Sintaxe dos valores

Estes três tipos aparecem em toda a configuração.

| Tipo | Aceita | Recusa |
|---|---|---|
| **duração** | sintaxe do Go — `500ms`, `30s`, `5m`, `1h30m`, `2h45m10s`; unidades `ns`, `us`, `ms`, `s`, `m`, `h`. Mais `d` (dia) e `w` (semana) sozinhos: `30d`, `2w`, `1.5d` | número pelado (`30`), forma misturando `d`/`w` (`1d12h`), unidade por extenso (`1 hora`) |
| **tamanho** | IEC `KiB`, `MiB`, `GiB` (×1024) e SI `KB`, `MB`, `GB` (×1000), com decimais: `32MiB`, `1.5GiB`, `512KB`. Inteiro pelado é bytes: `1048576` | decimal pelado (`1.5`), sufixos desconhecidos (`32M`, `32 mb`) |
| **tentativas** | inteiro positivo, ou a palavra `unlimited` — essa só em um `service`. `0` conta como "chave não definida", então o default entra | contagem negativa, `unlimited` em uma `task` |

Dois lugares recebem duração por HTTP em vez de YAML, e eles aceitam **só a sintaxe do Go**, sem
`d`/`w`: o campo `timeout` do `POST /v1/processes` e o parâmetro de query `grace` do
`DELETE /v1/processes/{id}` (`--grace` na CLI).

A decodificação é **estrita** em todo lugar: chave desconhecida é erro de carga, nunca um default
silencioso. Um typo em uma chave sensível como `allow_root_processes` tem que falhar alto.

## Configuração do daemon

`/etc/processd/processd.yaml`. Toda chave é opcional; os defaults abaixo são o que um arquivo
ausente entrega.

| Chave | Tipo | Default | Valores e regras |
|---|---|---|---|
| `listen` | string | `127.0.0.1:7373` | bind do HTTP. Sem TLS nativo — ponha um proxy na frente para expor. Não pode ser vazio |
| `data_dir` | caminho | `/var/lib/processd` | estado em SQLite |
| `log_dir` | caminho | `/var/log/processd` | arquivos de saída por tentativa |
| `workers_dir` | caminho | `/etc/processd/workers.d` | onde os `*.yaml` de worker são lidos |
| `max_processes` | int > 0 | `50` | teto do node inteiro |
| `shutdown_grace` | duração | `30s` | orçamento dado à árvore de processos no encerramento |
| `orphan_policy` | enum | `kill` | `kill` mata o processo que sobreviveu ao daemon antes do retry; `leave` mantém e não repete |
| `execution_mode` | enum | `workers` | `workers` só executa workers pré-configurados; `raw` aceita comando escolhido pelo cliente e então exige `allowed_commands` |
| `allowed_commands` | lista de caminhos | `[]` | caminhos absolutos, usados só no modo `raw` |
| `allow_root_processes` | bool | `false` | permite rodar sem `user` quando o daemon é root |
| `queue.max_depth` | int > 0 | `1000` | fila cheia responde `429` |
| `queue.item_ttl` | duração | `1h` | item que esperou mais que isso falha com `queue_timeout`. Um service nunca expira assim |
| `history.retention` | duração | `30d` | GC das execuções terminadas |
| `history.max_rows` | int | `500000` | teto de linhas retidas |
| `logs.max_bytes_per_stream` | tamanho > 0 | `32MiB` | cap por stream por tentativa; ao atingir, marca `log_truncated` |
| `logs.retention` | duração | `14d` | GC dos arquivos de log |
| `ui.enabled` | bool | `true` | console web em `/ui/` |
| `auth.tokens[].name` | string | — obrigatório | identifica o token na trilha de auditoria |
| `auth.tokens[].hash` | string | — obrigatório | `sha256:...`, de `processd setup` ou `processd token hash` |
| `auth.tokens[].workers` | lista | `[]` = todos | restringe o token a workers específicos |

## Definição de workers

Arquivos `*.yaml` em `workers_dir`, cada um com `version: 1` e uma lista `workers`. Um arquivo pode
declarar vários workers; o nome é chave do daemon, não do arquivo, e precisa ser único entre todos
eles.

Um `service` é um worker que não deveria terminar. Os defaults de restart vêm de graça; só a rotação
de log é obrigatória, porque uma única tentativa pode durar meses, encher o cap e depois emudecer:

```yaml
# /etc/processd/workers.d/api.yaml
version: 1
workers:
  - name: api
    type: service
    command: /usr/local/bin/api
    cwd: /srv/api
    user: api
    kill_grace: 30s
    retry:
      no_retry_exit_codes: [78]     # config inválida não vale reiniciar em loop
      reset_after: 10m
      on_shutdown: true             # volta a subir no próximo start do daemon
      backoff: { type: exponential, initial: 1s, max: 1m, jitter: 0.2 }
    logs:
      rotate: { max_files: 5 }
```

### Campos

| Chave | Tipo | Default | Valores e regras |
|---|---|---|---|
| `name` | string | — obrigatório | único entre todos os arquivos |
| `enabled` | bool | `true` | `false` ainda carrega o worker, e recusa execuções com `422` |
| `type` | enum | `task` | `task` termina e o sucesso é final; `service` não deve terminar e qualquer saída reinicia. O tipo é do worker — um request pode declará-lo, mas só para concordar |
| `command` | caminho | — obrigatório | **absoluto**, executado direto, nunca por shell |
| `args` | lista de strings | `[]` | pode conter `{{param}}`; a substituição nunca divide um elemento |
| `params` | mapa | `{}` | o que um request pode enviar (tabela abaixo) |
| `cwd` | caminho | `/` | precisa ser absoluto; diretório inexistente falha o start, não a carga |
| `user` | string | vazio | **nome** de usuário do sistema, não uid. Vazio com o daemon rodando como root recusa o start, a menos que `allow_root_processes: true` |
| `group` | string | grupo primário do usuário | nome de grupo do sistema; grupos suplementares são aplicados |
| `env` | mapa | `{}` | o ambiente do filho é construído, não herdado: o ambiente do daemon guarda segredos |
| `env_passthrough` | lista de strings | `[]` | nomes repassados do ambiente do daemon, ex.: `[PATH, LANG, TZ]` |
| `timeout` | duração | `0` = sem timeout | ao estourar: `SIGTERM` no grupo → `kill_grace` → `SIGKILL`, e o desfecho conta como o trigger de retry `timeout`. Recusado em um `service`: não há prazo a estourar. Um request só pode sobrescrever se `overridable` listar `timeout`, e esse valor é sintaxe do Go apenas |
| `kill_grace` | duração | `15s` | espera entre o `SIGTERM` e o `SIGKILL` |
| `max_processes` | int ≥ 0 | `0` = só o limite global | teto por worker. Uma task acima dele **espera na fila**; um service acima dele é recusado com `503`, nunca enfileirado |
| `lock` | string | vazio = sem lock | chave de exclusão mútua, pode conter `{{param}}` |
| `lock_conflict` | enum | `queue` | `queue` espera o lock liberar; `reject` responde `409` na hora |
| `overridable` | lista de enum | `[]` | o que um request pode sobrescrever: `env`, `timeout`, `lock`. Qualquer outro → `400` |
| `retry` | objeto | desligado | tabela abaixo |
| `logs.max_bytes_per_stream` | tamanho | o valor do daemon (`32MiB`) | cap por stream por tentativa; a retenção é do daemon |
| `logs.rotate.max_files` | int ≥ 0 | `0` = sem rotação | quantos arquivos rotacionados manter atrás do vivo. Sem rotação o stream para de gravar ao encher o cap — **obrigatório em um `service`** |

### `params`

Argumentos chegam ao processo **apenas** por params declarados e validados.

| Chave | Tipo | Default | Regra |
|---|---|---|---|
| `required` | bool | `false` | ausente no request → `400` |
| `pattern` | regex RE2 | vazio | compilada na carga do worker; valor fora dela → `400` |
| `enum` | lista de strings | `[]` | valor fora da lista → `400` |
| `default` | string | vazio | usado quando um param opcional não veio |

Regras de substituição:

1. `{{nome}}` é resolvido **dentro** de elementos de `args` e em `lock`, em nenhum outro lugar.
2. Nunca cria, divide ou junta elementos de argv: um valor com espaços continua sendo um argumento.
3. Placeholder não declarado em `params` **derruba a carga** — um typo nunca chega à linha de
   comando como um `{{id}}` literal.
4. Param enviado e não declarado → `400`.
5. Elemento de argv que é só um placeholder opcional ausente é removido, em vez de virar `""`.

### `retry`

`enabled` é tri-estado: ausente não é o mesmo que `false`. Uma `task` sem a chave não tenta de novo;
um `service` sem ela reinicia, e um `enabled: false` explícito em um service é recusado na carga.

| Chave | Tipo | Default (`task`) | Default (`service`) | Valores |
|---|---|---|---|---|
| `enabled` | bool | `false` | `true`, e não pode ser `false` | sem isso, uma tentativa e pronto |
| `max_attempts` | tentativas | `1` | `unlimited` | total, incluindo a primeira tentativa. `unlimited` só é aceito em um service |
| `retry_on` | lista de enum | `[nonzero_exit, signal, start_error]` | os mesmos mais `exit` | `nonzero_exit`, `signal`, `start_error`, `timeout`, `exit`. `exit` é qualquer saída, uma limpa incluída, e só um service pode usar |
| `success_exit_codes` | lista de int | `[0]` | nenhum, e declarar é recusado | código listado → `COMPLETED` |
| `no_retry_exit_codes` | lista de int | `[]` | `[]` | código listado → `FAILED` imediato, sem retry |
| `reset_after` | duração | `0` = desligado | `0` = desligado | tentativa que durou mais que isso zera o contador |
| `on_shutdown` | bool | `false` | `false` | `true` devolve a execução à fila no shutdown, em vez de cancelá-la |
| `backoff.type` | enum | `exponential` | `exponential` | `exponential`, `linear`, `fixed` |
| `backoff.initial` | duração | `5s` | `5s` | atraso da primeira repetição |
| `backoff.max` | duração | `5m` | `5m` | teto do atraso |
| `backoff.multiplier` | float > 0 | `2` | `2` | usado só por `exponential` |
| `backoff.jitter` | float 0..1 | `0` | `0` | espalha o atraso em `±jitter`; sem ele, tudo que falhou junto repete junto |

Curvas: `fixed` = `initial`; `linear` = `initial × tentativa`; `exponential` =
`initial × multiplier^(tentativa-1)`. O teto `max` é aplicado antes do jitter.

O lock é mantido entre tentativas: soltá-lo durante o backoff deixaria outra execução tomá-lo no
meio do retry.

## Ciclo de vida

```
CREATED  → QUEUED, STARTING, CANCELED
QUEUED   → STARTING, CANCELED, FAILED
STARTING → RUNNING, CRASHED
RUNNING  → COMPLETED, CRASHED, STOPPING
STOPPING → CANCELED, FAILED, CRASHED, QUEUED
CRASHED  → RETRYING, FAILED
RETRYING → STARTING, QUEUED, CANCELED
```

`COMPLETED`, `FAILED` e `CANCELED` são terminais e imutáveis — rodar o mesmo trabalho de novo é uma
execução nova, com ID novo. Uma transição que falta na tabela de `internal/core/state.go` é bug,
nunca um no-op silencioso. Os estados carregam um motivo: `user_request`, `timeout`, `max_attempts`,
`queue_timeout`, `shutdown`, `daemon_restart`, `start_error`, `no_retry_exit_code`, `lock_conflict`,
`orphaned`, `no_capacity`.

Um service nunca fica na fila: ele toma seu slot na admissão ou é recusado, e só passa por `QUEUED`
na volta de um restart do daemon que ele foi instruído a sobreviver.

## CLI

Um único binário, cliente da mesma API pública. `--server`/`PROCESSD_SERVER` e `--token`/
`PROCESSD_TOKEN` configuram; sem token explícito ele lê o arquivo gravado pelo `processd setup` ao
lado da configuração. Toda flag persistente tem a variável com prefixo `PROCESSD_`
(`--log-level` → `PROCESSD_LOG_LEVEL`).

| Comando | O que faz |
|---|---|
| `processd setup [--dry-run] [--rotate-token] [--listen addr] [--systemd=false] [--start=false] [--output json]` | instala o node: diretórios, configuração, token, unit systemd, e imprime tudo |
| `processd serve --config <path>` | sobe o daemon |
| `processd status` | saúde, versão, slots, rodando e na fila |
| `processd ps [--status S] [--type task\|service] [--worker w] [--limit n] [--cursor c] [--output table\|json]` | lista execuções |
| `processd run <worker> [--param nome=valor] [--lock k]` | cria uma execução |
| `processd logs <id> [--stream stdout\|stderr\|both] [--attempt n] [--tail n] [-f]` | saída capturada, com `-f` em streaming |
| `processd stop <id> [--grace 15s]` | `SIGTERM` no grupo, `SIGKILL` depois da graça |
| `processd restart <id> [--grace 15s] [--param n=v] [--wait 1m]` | para a execução e cria outra a partir da definição atual do worker |
| `processd signal <id> <SINAL>` | envia um sinal do allowlist ao grupo |
| `processd workers` | workers carregados, com os params declarados |
| `processd reload` | relê `workers.d` |
| `processd token hash` | lê um token de stdin e imprime o digest da configuração |

Sinais aceitos: `SIGTERM`, `SIGINT`, `SIGQUIT`, `SIGHUP`, `SIGUSR1`, `SIGUSR2`, `SIGKILL`,
`SIGSTOP`, `SIGCONT`. Qualquer outro é recusado com `400`, e todo sinal atinge o **process group**
inteiro — sinalizar só o PID deixaria netos vivos.

## API HTTP

Base `/v1`, JSON na entrada e na saída. Todo endpoint exige `Authorization: Bearer <token>`, exceto
`GET /v1/health`.

| Endpoint | Notas |
|---|---|
| `POST /v1/processes` | `{"worker":"...","params":{...}}`. `201` admitida, `202` enfileirada. Um `Idempotency-Key` opcional devolve a resposta original com `Idempotent-Replay: true` enquanto aquela execução estiver retida; a mesma chave com corpo diferente → `409` |
| `GET /v1/processes` | filtros: `status` (repetível), `type`, `worker`, `lock`, `created_after`, `created_before`; `limit` default 50, máximo 500; paginação por cursor |
| `GET /v1/processes/{id}` | representação completa, com CPU e memória ao vivo enquanto roda |
| `DELETE /v1/processes/{id}?grace=15s` | `CANCELED` com `reason: user_request`, e **nunca** dispara retry |
| `POST /v1/processes/{id}/signal` | `{"signal":"SIGUSR1"}` |
| `GET /v1/processes/{id}/logs` | `?stream=stdout\|stderr\|both&attempt=N&tail=N` |
| `GET /v1/processes/{id}/logs/stream` | Server-Sent Events, um evento `line` por linha, `end` quando a tentativa termina |
| `GET /v1/workers` | os workers que o token pode ver |
| `POST /v1/reload` | relê `workers.d`, tudo-ou-nada |
| `GET /v1/health[?deep=1]` | público; `deep` também pinga o store |
| `GET /v1/stats` | slots, profundidade da fila, estados, contadores de service |
| `GET /v1/metrics` | formato texto do Prometheus |

Os erros sempre têm a mesma cara:
`{"error":{"code":"param_invalid","message":"...","details":{...}}}`.

| Status | Quando |
|---|---|
| `400` | payload inválido, param inválido ou não declarado, `type` não suportado, sinal fora do allowlist |
| `401` / `403` | sem token válido / token sem permissão para aquele worker, ou comando livre em modo `workers` |
| `404` | worker ou execução inexistente |
| `409` | lock ocupado com `lock_conflict: reject`, sinal em execução que não está rodando, chave de idempotência reusada com corpo diferente |
| `422` | worker desabilitado |
| `429` | fila cheia (`queue.max_depth`) |
| `503` | daemon encerrando, ou service sem slot livre (`no_capacity`) |

## Métricas

```text
processd_daemon_up
processd_slots_used / processd_slots_max
processd_workers
processd_running_attempts
processd_queue_depth
processd_processes{state}
processd_processes_running{worker}
processd_processes_queued{worker}
processd_processes_total{worker,status}        # contador
processd_process_attempts_total{worker}        # contador
processd_service_restarts_total{worker}        # contador
processd_process_duration_seconds{worker}      # histograma
processd_running_cpu_seconds{worker}
processd_running_rss_bytes{worker}
```

Os contadores e o histograma vivem em memória e zeram com o daemon — que é o que o Prometheus espera
de um contador local de processo: um restart aparece como reset, não como buraco. CPU e memória são
amostradas de `/proc/<pid>/stat` no momento da leitura, sempre conferindo antes o start time do
processo, porque PIDs são reciclados.

## Segurança

Os processos são iniciados com `exec.Command(cmd, args...)`, nunca `sh -c`. No `execution_mode:
workers` default, só workers pré-configurados executam, e argumentos chegam neles apenas por params
validados por regex ou enum. Autenticação por token é obrigatória, o bind default é `127.0.0.1`, o
ambiente do filho é construído em vez de herdado, cada processo ganha seu próprio process group, e
nada roda como root sem opt-in explícito.

## Princípios de design

* **Simple by default** — sem Redis, Kafka ou Kubernetes.
* **API-first** — a CLI é cliente da mesma API pública.
* **Single binary** — instalar é copiar um arquivo.
* **Process-first** — a abstração é processo e execução, não container.
* **Fail closed** — na dúvida, recusa: sem token, sem `user` rodando como root, ou com param fora do
  padrão declarado, nada executa.
* **Linux-first** — depende de process groups POSIX, sinais e `/proc`.

## Roadmap

| Fase | Entrega |
|---|---|
| 1 ✅ | process manager local: API, estados, persistência, auth |
| 2 ✅ | supervisor: fila, locks, retry/backoff, timeout, recovery |
| 3 ✅ | observabilidade: métricas, streaming de logs, CPU/memória, console web |
| 4 ✅ | `type: service`: restart contínuo e rotação de log |
| 5 | agents e execução distribuída |

---

## Desenvolvimento

Requer Go 1.25+ e Linux.

```bash
make install-tools     # golangci-lint e govulncheck
make build             # bin/processd
make test              # go test ./...
make test-race         # com o detector de corrida
make test-integration  # ponta a ponta: daemons e processos reais
make lint              # golangci-lint
make fmt               # formatação
make audit             # govulncheck
make release-check     # valida .goreleaser.yml
make release-snapshot  # archives, pacotes e SBOMs em dist/, sem publicar
```

Layout:

```
cmd/processd/        entrypoint
internal/cli/        árvore de comandos cobra, cliente da própria API
internal/daemon/     grafo de dependências e ciclo de vida
internal/api/        handlers HTTP, auth, contrato de erro
internal/core/       domínio e máquina de estados, sem I/O
internal/config/     configuração do daemon e definição de workers
internal/queue/      admissão, slots, backoff
internal/supervisor/ supervisão de cada execução
internal/runner/     exec, process groups, sinais, /proc
internal/logstore/   arquivos de log por tentativa, leitura e streaming
internal/metrics/    contadores e histogramas no formato texto do Prometheus
internal/webui/      console web embutido (go:embed)
internal/store/      interface de persistência + SQLite
```

As releases são automáticas: um push de tag `v*` dispara
[`.github/workflows/release.yml`](.github/workflows/release.yml), que roda os testes e o GoReleaser,
e publica os archives, os pacotes `.deb`/`.rpm`/`.apk`, um SBOM SPDX por artefato, o `checksums.txt`
e a assinatura cosign dele.

```bash
git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0
```

Exemplos de configuração e unit systemd em [`examples/`](examples/).

## Licença

[Apache 2.0](LICENSE)
