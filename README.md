# Processd

Process manager leve, em Go, que executa e supervisiona processos CLI através de uma REST API.

> **Run any CLI process through a simple API and keep it alive.**

Fica no espaço entre supervisores locais (Supervisor, PM2) e orquestradores distribuídos
(Nomad, Kubernetes): um binário, um arquivo de config, uma API.

---

## Status

**Pré-alpha.** O esqueleto em Go existe e compila; o daemon sobe, autentica e valida requisições,
mas ainda não executa processos — persistência, fila e supervisão estão como stubs marcados com
`TODO(spec §…)`.

| Área | Estado |
|---|---|
| API REST, auth por token, contrato de erro | implementado |
| Config do daemon e workers (YAML estrito) | implementado |
| Validação de params e montagem de argv | implementado |
| Execução: process group, uid/gid, env limpo, sinais, timeout | implementado |
| Captura de logs por tentativa com limite de tamanho | implementado |
| Fingerprint `(pid, starttime)` contra reciclagem de PID | implementado |
| Backoff com jitter, contagem de slots | implementado |
| Máquina de estados | implementada |
| Persistência SQLite (schema e migrations prontos, CRUD pendente) | **pendente** |
| Scheduler, fila, locks | **pendente** |
| Supervisão de execuções, retry, reconciliação | **pendente** |
| Leitura de logs pela API | **pendente** |

A spec completa está em **[docs/SPEC.md](docs/SPEC.md)**.

---

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

Hoje respondem de verdade: `status`, `workers`, `reload`, `token hash`, e a validação completa de
`POST /v1/processes` (que ainda falha na hora de enfileirar).

---

## Desenvolvimento

Requer Go 1.27+ e Linux.

```bash
make install-tools   # golangci-lint e govulncheck
make build           # bin/processd
make test-race       # testes com detector de corrida
make lint            # golangci-lint
make fmt             # formatação
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
| 1 | Process manager local: API, estados, persistência, auth |
| 2 | Supervisor: fila, locks, retry/backoff, timeout, recovery |
| 3 | Observabilidade: métricas, streaming de logs, histórico |
| 4 | `type: service` e desired state local |
| 5 | Agents e execução distribuída |

---

## Licença

[Apache 2.0](LICENSE)
