# migration_script — `drivemig`

Migração de conteúdo do Google Drive entre contas, em Go. Reescrita do par de
scripts Python `pre-migracao.py` / `migra-drive.py` com folder-cache,
concorrência limitada, retry/backoff classificado, timeout, resume e logs
estruturados.

## Auth híbrida (por necessidade técnica)

| Tipo de conta            | Credencial | Por quê |
|--------------------------|------------|---------|
| `@gmail.com` (pessoal)   | **OAuth** (token em arquivo local, `--token-dir`) | DWD não impersona contas pessoais |
| Workspace (`@dominio`)   | **DWD** (service account JSON) | impersonação via `Subject` |

A escolha é automática pelo domínio do email. O **uso é headless**: o run só
lê o token já provisionado em disco e renova o access token sozinho (gravando
de volta no arquivo se o refresh token rotacionar).

> **Deploy:** roda numa **VM Linux dedicada** a essa migração — sem misturar com
> outras workloads. Credenciais ficam **no próprio host** (sem Secret Manager,
> pra economizar custo). Proteja-as: `tokens/` em `0700`, arquivos de token em
> `0600` (o binário já grava assim), e o disco da VM idealmente criptografado.

## Pré-requisitos (uma vez)

1. **OAuth Client** do tipo *"TVs and Limited Input devices"* no projeto GCP.
   Baixe o `client_secret.json` e coloque no host (passe via `--client-secret`).
2. **Service Account com DWD** habilitado (escopo `.../auth/drive`) para o
   domínio Workspace de destino; coloque a key no host (passe via `--sa-json`).
3. Consentir cada conta `@gmail` de origem **uma vez** (device flow):
   ```
   drivemig auth voce@gmail.com --client-secret client_secret.json --token-dir tokens
   ```
   Abra a URL impressa em `google.com/device`, digite o código, autorize. O token
   fica salvo em `tokens/<email>.json`.

## Uso

```
# 1) mapear origem + compartilhar + gerar CSVs (dry-run primeiro)
drivemig prepare --csv pares.csv --dry-run

# 2) aplicar (dry-run, depois real)
drivemig apply --folders out_folders.csv --manifest manifest.csv \
    --workers 8 --state-file state.jsonl --dry-run
drivemig apply --folders out_folders.csv --manifest manifest.csv \
    --workers 8 --state-file state.jsonl
```

`pares.csv`:
```csv
email_origem,email_destino
voce@gmail.com,voce@empresa.com.br
```

### Flags principais

`prepare`: `--csv` (obrigatório), `--folder-mode create_or_reuse|must_exist`,
`--role`, `--reuse-folders`, `--share-failures`, `--dry-run`.

`apply`: `--folders`/`--manifest` (obrigatórios), `--workers N`,
`--update-changed "" |skip|replace|keep-both`, `--only-dest`/`--only-src`,
`--state-file` (resume), `--dry-run`.

Comuns: `--sa-json` (key DWD), `--client-secret` (OAuth client JSON),
`--token-dir` (tokens por conta), `--timeout`, `--log-dir`, `--log-level`.

## Nota sobre quota

O teto de velocidade é a quota **por usuário** do Drive (`userRateLimitExceeded`
/ `sharingRateLimitExceeded`), não o número de workers. Múltiplas SAs ou
rotação de IP **não** aumentam esse limite. Ganho real vem do folder-cache e de
não fazer `files.get` por arquivo. Mantenha `--workers` modesto (~8) e deixe o
backoff trabalhar.
