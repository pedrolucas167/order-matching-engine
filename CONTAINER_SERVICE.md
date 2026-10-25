# Serviço de ordens em contêiner

A imagem executa a API de ordens com armazenamento durável. Não roda os cenários da demo nem cria negócios automaticamente. Instrumentos e contas são configurados pelo operador; ordens reais recebidas pela API são processadas pelo matching do simulador, com margem, posições, recibos idempotentes e outbox. Não há conexão a uma bolsa ou corretora.

## Construir e executar

```powershell
docker build --platform linux/amd64 -t order-matching-engine:local .
Copy-Item .env.example .env
```

Preencher `ORDER_API_TOKEN` no arquivo `.env` com um segredo aleatório de pelo menos 32 caracteres, por exemplo 32 bytes gerados por um gerador criptográfico e codificados em hexadecimal. O token dá acesso de operador a todas as contas, não identifica clientes individuais. `.env` é ignorado pelo Git e excluído do contexto de construção.

Revisar `config/engine.example.json` antes do uso. O arquivo contém dois clientes e um instrumento em unidades de simulação, mas não submete nenhuma ordem. Preços, quantidades e valores monetários da configuração e da API são strings decimais de inteiros na escala acordada pelo operador.

```powershell
docker compose up -d
docker compose ps
docker compose logs matching
```

O Compose publica somente `127.0.0.1:8080`, monta a configuração como somente leitura e usa um volume nomeado para `/data`. A imagem roda como UID/GID 65532, sem shell, com o sistema de arquivos de execução somente leitura e sem capabilities no Compose. Configurações montadas precisam ser legíveis por esse usuário. Segredos também podem ser fornecidos por arquivo com `-token-file`, com permissão de leitura apropriada.

Para parar, usar `docker compose down`. O volume permanece para a próxima inicialização. A opção `down -v` remove o banco e não deve ser usada quando se deseja preservar os dados. A garantia inicial só é aplicada a clientes ainda inexistentes; reiniciar ou mudar `initial_collateral` no arquivo não substitui o saldo de uma conta já cadastrada. Mudança incompatível de moeda ou margem configurada impede o início do serviço.

O binário também pode rodar fora do contêiner:

```powershell
go run ./cmd/order-server -listen 127.0.0.1:8080 -config config/engine.example.json -db data/engine.db
```

Nesse caso, criar o diretório `data` e definir `ORDER_API_TOKEN` no ambiente antes de iniciar. Os parâmetros `-order-buffer`, `-trade-buffer` e `-shutdown-timeout` controlam admissão, retenção de negócios não confirmados e prazo de parada graciosa. O servidor tem limites de tamanho de cabeçalhos/corpos e timeouts de HTTP. A exposição fora de uma rede controlada exige terminação TLS e autorização adequada; o token atual é uma credencial única de operador.

## Endpoints

Todos os endpoints financeiros exigem `Authorization: Bearer <token>`. Apenas `GET /healthz` é público e retorna disponibilidade, sem dados financeiros. `POST /orders` também exige uma única `Idempotency-Key`.

| Método e caminho | Função |
| --- | --- |
| `POST /orders` | Submeter uma ordem e obter o recibo idempotente |
| `DELETE /orders/{symbol}/{order_id}` | Cancelar ordem ou stop ainda ativo |
| `GET /books/{symbol}` | Consultar as melhores ofertas e quantidades |
| `GET /accounts/{client_id}/margin` | Consultar garantia, patrimônio, margem e P&L |
| `GET /accounts/{client_id}/positions/{symbol}` | Consultar posição e contratos pendentes |
| `POST /marks` | Confirmar uma referência manual de preço pelo operador |
| `GET /trades/state` | Consultar os cursores da outbox |
| `GET /trades?after=0&limit=100` | Ler eventos posteriores ao cursor |
| `POST /trades/ack` | Confirmar cumulativamente uma sequência já processada |

Exemplo de ordem para uma conta configurada:

```json
{
  "order_id": "709b5833-fdcb-4c3d-bfa7-1dfec7b566af",
  "client_id": "755b2162-9ab3-4a0b-a48f-571def953639",
  "symbol": "SIM-FUT",
  "side": "buy",
  "type": "limit",
  "quantity": "1",
  "price": "100"
}
```

HTTP 200 na submissão informa que a solicitação foi processada; o resultado financeiro está em `initial_status`, `filled_quantity`, `remaining_quantity` e `rejection_reason`. Reenviar a mesma chave e os mesmos dados recupera o recibo original, inclusive depois de recriar o contêiner com o mesmo volume. Não alterar chave ou ID apenas por ter ocorrido timeout.

O consumidor precisa ler a outbox, aplicar cada negócio de forma idempotente pelo ID e depois confirmar `{"sequence":"1"}` em `/trades/ack`. Negócios não confirmados são republicáveis após reinício. Ao atingir o limiar da outbox, novas ordens recebem backpressure; não existe consumidor interno que descarte ou confirme eventos automaticamente.

A referência manual usa `{"symbol":"SIM-FUT","price":"110"}` em `/marks`. Cabe ao operador assegurar que o preço é atual. O polling externo ainda não está integrado ao perfil durável. A liquidação automática é opcional no arquivo de configuração:

```json
{
  "max_slippage_bps": 100,
  "max_mark_age": "5s",
  "retry_interval": "1s"
}
```

Esse objeto é o campo `liquidation` do arquivo. As referências recuperadas do banco não autorizam liquidação até uma nova marcação. O modelo de margem continua simplificado e os detalhes de consistência estão no [contrato durável](DURABLE_RECOVERY.md).

## Publicação no GitHub Packages

O workflow verifica testes, detector de corridas, cobertura mínima de 95%, `go vet`, dependências e o comportamento do contêiner real. Depois de um push na branch principal, o job de publicação carrega **a imagem que foi testada** e a envia ao GitHub Container Registry com:

```text
ghcr.io/pedrolucas167/order-matching-engine:latest
ghcr.io/pedrolucas167/order-matching-engine:sha-<commit>
```

PRs executam as validações sem publicar. O workflow usa `GITHUB_TOKEN` com permissão de escrita em Packages somente no job de publicação. A imagem tem rótulos de origem e revisão, e inclui avisos de licença das dependências. O runtime é Linux/amd64; a base de compilação está fixada por digest.

Após a publicação, definir `ORDER_IMAGE` no `.env` com a referência do registry, executar `docker compose pull` e iniciar o serviço. Para implantação fixada, usar o digest retornado pelo registry. Um pacote novo no GHCR normalmente começa privado; download privado exige login no registry com acesso de leitura ao pacote. Essa credencial do GitHub é diferente de `ORDER_API_TOKEN` e não deve ser usada como token da API.

O teste de contêiner pode ser reproduzido depois de construir a imagem:

```powershell
$env:ORDER_CONTAINER_IMAGE='order-matching-engine:local'
go test -run '^TestContainerService' -v ./internal/server
```

Ele submete e executa ordens, atualiza marcação, confirma negócios, remove o contêiner e cria outro com o mesmo volume. Verifica autenticação, usuário não privilegiado, healthcheck, parada graciosa, preservação dos recibos, posições, patrimônio e cursores de confirmação.

As medições anteriores de latência de dez mil chamadas pertencem ao perfil em memória. Não são um SLA desta imagem, que sincroniza o disco. Histórico incremental, replicação, contas com credenciais individuais e integração de cotações externas permanecem como evolução.
