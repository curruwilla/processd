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
| Métricas Prometheus em `/v1/metrics` | pronto |
| CLI completa | pronto |

Fora do MVP, por decisão: `type: service` e desired state, streaming de logs, execução distribuída
(Agents), TLS nativo, Web UI. Ver [docs/SPEC.md](docs/SPEC.md) §22 e §25.

## O que faz

* Executa processos CLI via REST API, com argumentos validados.
* Identifica cada execução por um ID lógico estável, independente do PID.
* Limita processos simultâneos (global e por worker) e enfileira o excedente.
* Captura stdout/stderr, exit code e sinal de término.
* Aplica timeout, retry com backoff e locks contra execução concorrente.
* Persiste estado em SQLite e reconcilia após restart.
* Faz graceful shutdown do daemon e da árvore de processos.

## O que não faz

Orquestração de containers, service mesh, consenso distribuído, auto-scaling, provisionamento.
Não substitui o systemd para serviços de sistema.

---

## Uso

Define um worker:

```yaml
# /etc/processd/workers.d/invoice.yaml
version: 1
workers:
  - name: invoice-process
    type: task
    command: /usr/bin/php
    args: ["/var/www/app/artisan", "invoice:process", "--id={{id}}"]
    params:
      id: { required: true, pattern: "^[0-9]{1,12}$" }
    cwd: /var/www/app
    user: www-data
    max_processes: 20
    timeout: 30m
    retry:
      enabled: true
      max_attempts: 5
      backoff: { type: exponential, initial: 5s, max: 5m, jitter: 0.2 }
```

Sobe o daemon e dispara uma execução:

```bash
processd serve --config /etc/processd/processd.yaml

curl -X POST http://127.0.0.1:7373/v1/processes \
  -H "Authorization: Bearer $PROCESSD_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"worker":"invoice-process","params":{"id":"123"}}'
```

```json
{ "id": "proc_01KABCDEF...", "status": "RUNNING", "pid": 18231, "attempt": 1 }
```

Acompanha:

```bash
processd status
processd workers
processd ps
processd logs proc_01KABCDEF...
processd stop proc_01KABCDEF...
```

---

## Desenvolvimento

Requer Go 1.27+ e Linux.

```bash
make install-tools     # golangci-lint e govulncheck
make build             # bin/processd
make test-race         # testes com detector de corrida
make test-integration  # testes ponta a ponta (sobe daemons e processos reais)
make lint              # golangci-lint
make fmt               # formatação
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
internal/logstore/   arquivos de log por tentativa
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
| 3 | Observabilidade: streaming de logs, CPU/memória, Web UI |
| 4 | `type: service` e desired state local |
| 5 | Agents e execução distribuída |

---

## Licença

[Apache 2.0](LICENSE)
