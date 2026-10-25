# Order Matching Engine

Um motor de matching de ordens de alta performance em Go, projetado para latência em microssegundos com tratamento robusto de concorrência.

## Funcionalidades

### Core Functionality
- **Price-Time Priority (FIFO)**: Ordens são matcheadas pelo melhor preço; no mesmo preço, seguem a ordem de processamento no loop
- **Limit & Market Orders**: Suporte para ambos os tipos de ordens
- **Publicação de Trades em Tempo Real**: Trades publicados via canais bufferizados
- **Multi-Symbol Support**: Order books separados por símbolo, processados pelo mesmo loop

### Concorrência e Performance
- **Dono único do matching**: Um loop processa comandos de novas ordens, cancelamentos, consultas e verificações periódicas de stops para todos os símbolos.
- **Cópias independentes**: Entradas, snapshots, níveis de preço e ordens retornadas não compartilham estado mutável com o chamador.
- **Backpressure**: `SubmitOrder` retorna `ErrBackpressure` se a fila de comandos estiver cheia ou a publicação de trades estiver atrasada.
- **Publicação assíncrona**: O loop publica trades em ordem, sem bloquear cancelamentos ou consultas por falta de consumidor.

## Arquitetura

### Estruturas de Dados
- **Order**: Representa uma ordem de trading com ID, símbolo, lado, tipo, preço, quantidade
- **OrderBook**: Mantém bids (max-heap) e asks (min-heap) para um único símbolo
- **PriceLevel**: Agrupa ordens no mesmo preço com ordenação FIFO
- **Trade**: Representa um trade matcheado com detalhes de execução

### Validação de novas ordens

`SubmitOrder` valida uma cópia da entrada antes de consultar o estado de encerramento ou backpressure e antes de enfileirar o comando. Uma entrada inválida retorna imediatamente um erro identificável por `errors.Is(err, ErrInvalidOrder)`, com a indicação do campo inválido. Ela não altera a entrada do chamador, books, stops, contadores ou estado de risco.

| Campo | Regra para uma nova submissão |
| --- | --- |
| Ordem e `ID` | Ordem não nula e UUID diferente de `uuid.Nil` |
| `Symbol` | Não vazio e sem espaços em branco nas extremidades; não há normalização automática |
| `Side` | `Bid` ou `Ask` |
| `Type` | `Limit`, `Market`, `StopMarket` ou `StopLimit` |
| `Quantity` e `Remaining` | Quantidade positiva e `Remaining == Quantity` |
| `Status` | `Pending` (valor padrão) ou `Open` |
| `Price` | Positivo para `Limit` e `StopLimit`; opcional nos tipos market, cujo preço de execução vem do book |
| `StopPrice` | Positivo para `StopMarket` e `StopLimit`; ignorado nos demais tipos |

`ClientID` continua opcional e não precisa estar cadastrado para esta validação estrutural. `Timestamp` também é opcional; a prioridade FIFO depende da ordem de processamento. Não foram adicionados limites arbitrários de tamanho, preço ou formato do símbolo além das regras acima. Limites financeiros continuam sob responsabilidade do RiskManager. O preço informado nos tipos market é preservado e pode participar das verificações de risco existentes, embora não determine o preço da execução; essas políticas não foram alteradas.

Essa entrada aceita novas ordens, não a restauração de ordens já parcialmente executadas. Fills parciais, ativações de stops e cópias de books continuam sendo processados internamente sem reaplicar a regra de nova submissão. Entrada válida ainda pode receber `ErrEngineClosed` ou `ErrBackpressure`; após aceitação na fila, duplicidade de ID ativo e risco continuam sendo verificados pelo loop.

### Perfil de futuros com margem inicial

`NewDerivativesMatchingEngine` cria um perfil financeiro separado da simulação educativa de `NewMatchingEngine`. O perfil inicial representa futuros simplificados, com contratos inteiros, posição líquida com sinal por cliente/instrumento e margem inicial fixa por contrato. Não há pagamento do valor nocional integral para abrir a posição.

- Registre cada `DerivativeInstrument` com símbolo, `MarginCurrency` e `InitialMarginPerContract` positivo. A configuração é fixa neste incremento; registrar novamente o mesmo valor é idempotente.
- Cadastre os clientes com `Collateral` e `MarginCurrency` correspondente. Clientes e instrumentos desconhecidos são rejeitados pelo risco, inclusive quando a entrada estrutural é válida.
- `AddClient` agora retorna erro. Atualizações de aporte não podem deixar o patrimônio abaixo da margem inicial exigida ao retirar recursos, e a moeda da conta é imutável; também não reiniciam o volume diário. Use `ResetDailyVolume` para uma mudança explícita de período.
- Para cada instrumento, a margem cobre a maior posição líquida alcançável entre `posição + compras pendentes` e `posição - vendas pendentes`. Uma ordem que fecha uma posição não libera a margem da posição antes de executar. Não há compensação entre instrumentos ou conversão de moedas.
- Ordens limit, market e stops reservam margem pela quantidade de contratos. O preço opcional de market não define esse orçamento. Stops comprometem margem enquanto estão pendentes e mantêm a reserva ao ativar.
- Fills convertem margem de ordens em margem de posição. Cancelamentos e rejeições do restante liberam somente o compromisso não executado; encerrar a posição libera sua margem. Autonegociação no mesmo cliente é recusada neste perfil, preservando a ordem passiva.
- `RiskLimits.MaxOrderSize` e `MaxDailyVolume` continuam sendo aplicados. O volume de ordens pendentes entra no orçamento diário, e `MaxPositionSize > 0` limita a maior posição potencial por instrumento. `MaxExposure` pertence ao modelo legado de exposição nocional e não controla este perfil de margem fixa.
- Quantidades e posições potenciais devem caber na faixa simétrica de `int64`; multiplicações e somas de margem são verificadas antes da reserva. Garantias e margens usam unidades inteiras da `MarginCurrency`. Um restante que não caiba no total do nível do book é rejeitado e sua reserva é liberada.
- `GetDerivativePosition` e `GetMarginAccount` retornam valores independentes. Use esses métodos para o perfil financeiro; os campos globais legados `Client.Exposure`, `LongPosition` e `ShortPosition` não representam posições de derivativos.

Exemplo de configuração dentro do pacote (com `fmt` e `github.com/google/uuid` importados):

```go
func derivativesExample() error {
    engine := NewDerivativesMatchingEngine(1000, 1000)
    defer engine.Close()
    if err := engine.AddDerivativeInstrument(DerivativeInstrument{
        Symbol: "SIM-FUT", MarginCurrency: "SIM", InitialMarginPerContract: 100,
    }); err != nil {
        return err
    }
    clientID := uuid.New()
    if err := engine.GetRiskManager().AddClient(&Client{
        ID: clientID, Collateral: 500, MarginCurrency: "SIM",
    }); err != nil {
        return err
    }
    order := &Order{
        ID: uuid.New(), ClientID: clientID, Symbol: "SIM-FUT",
        Side: Ask, Type: Limit, Price: 10000, Quantity: 2, Remaining: 2,
    }
    if err := engine.SubmitOrder(order); err != nil {
        return err
    }
    stats := engine.GetStats()
    position, _ := engine.GetRiskManager().GetDerivativePosition(clientID, "SIM-FUT")
    account, _ := engine.GetRiskManager().GetMarginAccount(clientID)
    fmt.Printf("Queued: %d, pending sells: %d, reserved margin: %d\n",
        stats.OrdersProcessed, position.OpenSellContracts, account.OrderMargin)
    return nil
}
```

O exemplo usa uma unidade monetária fictícia. Antes de executar, a ordem de venda deixa dois contratos pendentes e compromete 200 unidades de margem, embora seu nocional seja maior que a garantia. Ao executar, a posição líquida passa a negativa. O retorno de `SubmitOrder` continua indicando aceitação na fila; aprovação financeira é posterior.

O perfil também calcula P&L por FIFO, marcação e variação de liquidação conforme descrito abaixo. Liquidação automática e feed externo são recursos opcionais, configurados explicitamente. Ainda não implementa taxas, modelos de carteira como SPAN, opções ou uma clearing. Atualizar limites de tamanho ou volume afeta novas admissões; o gatilho de liquidação é o déficit de margem de manutenção.

No perfil de derivativos, `Close` continua imediato e sem flush: libera reservas das ordens pendentes e conserva a margem das posições já executadas. Não encerra posições nem garante a entrega dos trades ainda não publicados. Persistência e recuperação permanecem fora deste incremento.

### P&L, marcação e margem de manutenção

- `ContractMultiplier` define o valor monetário de cada unidade inteira de `Price`, por contrato; seu padrão é 1. Preços e margem precisam usar escalas compatíveis definidas pelo simulador. Não há ponto flutuante nem arredondamento de preço médio: o custo de entrada é mantido em lotes FIFO.
- `MaintenanceMarginPerContract` deve ser positiva e não exceder a margem inicial. Se omitida, assume a margem inicial, preservando a configuração anterior sem inventar um percentual de bolsa.
- `GetDerivativePosition` informa resultado realizado e não realizado; `GetDerivativeLots` devolve uma cópia dos lotes de entrada. Fechamentos parciais consomem os lotes mais antigos, e uma reversão abre o restante no preço do novo negócio.
- A primeira execução fornece a marcação inicial do instrumento. Depois disso, ela **não acompanha automaticamente os trades**. `SetDerivativeMarkPrice(symbol, price)` atualiza a referência explicitamente, pelo loop do motor, e `GetDerivativeMarkPrice` permite consultá-la. O feed externo opcional descrito abaixo assume a autoridade sobre a referência do instrumento vinculado.
- `SettleDerivativeInstrument(symbol, settlementPrice)` simula a variação de liquidação: move o resultado não realizado para o realizado e redefine a base dos contratos ainda abertos nesse preço. Repetir o mesmo preço não credita ou debita o resultado duas vezes. A chamada é manual; não existe calendário ou execução diária automática.
- `MarginAccount.Equity = Collateral + RealizedPnL + UnrealizedPnL`. `Collateral` representa o aporte-base administrado pelo simulador, e não é reescrito como lucro ou prejuízo. Não há protocolo bancário de depósitos, saques ou transferência para uma clearing.
- Patrimônio e resultados monetários usam a faixa simétrica de `int64`. No perfil financeiro, aportes e margem comprometida devem caber em `MaxInt64`; o perfil educativo legado e os preços continuam com seus tipos anteriores. Intermediários de soma usam precisão inteira ampliada antes de validar o resultado final, evitando dependência da ordem de iteração dos instrumentos. Marcação ou liquidação que exceda a faixa é rejeitada integralmente, sem alterar contas, lotes ou referência.
- `InitialMarginShortfall` e `MaintenanceMarginShortfall` mostram os déficits em relação às margens inicial e de manutenção. Patrimônio negativo continua visível, inclusive após encerrar uma posição; não há underflow nem perdão automático do saldo negativo.

**Política conservadora de insuficiência:** após processar uma ordem, um lote de stops ou uma marcação/liquidação, o motor cancela todas as ordens e stops pendentes das contas cujo patrimônio não cobre a margem inicial exigida. A partir daí, somente uma ordem `Market` que reduza uma posição existente, sem ultrapassar sua quantidade e sem outras ordens pendentes nesse instrumento, pode ser aceita apesar do déficit. Esse fechamento não é bloqueado pelo limite diário de volume ou posição; limites operacionais de tamanho da ordem e circuit breaker continuam aplicáveis. Cancelamento e fechamento não apagam perdas.

Ordens previamente admitidas são processadas como uma unidade pelo matching. Preços de execução distintos da marcação podem produzir déficit durante esse processamento; negócios já executados não são desfeitos. A política não promete impedir perdas por gaps. A proteção de preço da liquidação abaixo não altera as ordens market comuns.

`AddClient` pode atualizar o aporte-base: reforços são aceitos mesmo quando apenas reduzem o déficit; reduções são recusadas se o patrimônio resultante não cobrir a margem inicial. A moeda de uma conta cadastrada é imutável neste modelo. Sem configurar `SetLiquidationPolicy`, posições não são liquidadas automaticamente.

### Liquidação automática opcional

`SetLiquidationPolicy(LiquidationPolicy{MaxSlippageBps: 100, MaxMarkAge: 5*time.Second, RetryInterval: time.Second})` habilita tentativas de fechamento quando a conta apresenta déficit de **margem de manutenção**. Os valores do exemplo são parâmetros de simulação, não regras de bolsa. Uma política vazia desabilita o recurso. A avaliação ocorre após comandos e no ticker do motor; cada conta/instrumento respeita o intervalo entre tentativas.

Antes de tentar fechar posições, o motor cancela as pendências das contas com déficit de margem inicial. As contas são percorridas por ID e os instrumentos por símbolo, com nova verificação do déficit antes de cada instrumento. A tentativa usa a posição atual inteira, sem inverter seu sinal. Se ela exceder o limite operacional de tamanho, a tentativa fica bloqueada, sem fracionamento automático.

O fechamento é imediato, com resto cancelado, usando somente ofertas reais do book e o mesmo caminho de contabilização dos negócios normais. Para fechar uma compra, o preço mínimo é `ceil(mark*(10000-bps)/10000)`, pelo menos uma unidade; para fechar uma venda, o máximo é `floor(mark*(10000+bps)/10000)`. `bps` fica entre zero e 10000. Não há fill sintético no preço de referência. Falta de ofertas, preço fora da banda, autonegociação, falha de contabilização, circuit breaker ou limite operacional podem impedir o fechamento. A tentativa também aguarda quando a publicação de trades está congestionada.

Execuções parciais liberam somente a reserva do resto e mantêm a posição residual. Novas tentativas reavaliam posição, patrimônio, preço e liquidez. O volume e o limite de posição não impedem redução de risco; tamanho máximo e circuit breaker continuam aplicáveis. `GetLiquidationStatus(clientID, symbol)` devolve uma cópia do resultado da **última tentativa**, com quantidade tentada, executada, restante, limite, horário e motivo; não é um histórico acumulado. Um depósito posterior não apaga esse registro.

Todos os instrumentos com posição aberta na conta precisam de referências explícitas dentro de `MaxMarkAge`. A marcação inicial derivada de um negócio não basta para autorizar liquidação. Feed parado, inválido ou atrasado bloqueia novas tentativas. Contas afetadas por uma execução podem ser avaliadas no próximo ciclo; não existe cascata recursiva. Saldo negativo persiste após fechar a posição: não há perdão de dívida, fundo de seguro ou garantia de execução.

### Alimentação externa de preços

`RunMarkPriceFeed(ctx, config)` executa polling HTTP do endpoint público de mark price de futuros USD-M da Binance e bloqueia até cancelar o contexto ou fechar o motor. Deve ser chamado em uma goroutine cujo resultado o chamador acompanha. Não usa credenciais nem envia ordens a uma bolsa. O contrato de resposta segue a [documentação oficial de market data](https://developers.binance.com/en/docs/catalog/core-trading-derivatives-trading-usd-s-m-futures/api/rest-api/market-data#mark-price).

```go
config := MarkPriceFeedConfig{
    Symbol: "BTC-SIM", SourceSymbol: "BTCUSDT",
    PriceScale: 100_000_000,
    PollInterval: time.Second, MaxAge: 5 * time.Second,
    MaxFutureSkew: 100 * time.Millisecond, RequestTimeout: 2 * time.Second,
}
// Após cadastrar BTC-SIM, usar a mesma unidade inteira nos preços e na contabilidade.
// O chamador fornece ctx, inicia a goroutine e acompanha seu erro de retorno.
err := engine.RunMarkPriceFeed(ctx, config)
```

`BaseURL` vazio usa `https://fapi.binance.com`. Outros endereços precisam de HTTPS; HTTP é aceito somente em loopback para testes locais. Não há redirects. Polling fora de loopback exige intervalo de pelo menos um segundo; o chamador deve considerar limites agregados do provedor. Erros HTTP, incluindo restrição regional e rate limit, suspendem o instrumento; não há fallback automático. Respostas são limitadas a 16 KiB e cada requisição tem timeout.

`PriceScale` converte o decimal para a unidade inteira do motor, sem ponto flutuante nem arredondamento. Símbolo divergente, decimal inválido, precisão não representável, preço zero, overflow, horário ausente, futuro além da tolerância, dado antigo ou fora de sequência são rejeitados. Uma resposta idêntica com o mesmo horário é idempotente e não renova a idade da fonte; um preço diferente no mesmo horário é inválido.

Vincular um feed cancela ordens e stops do instrumento até chegar a primeira referência válida. Falha ou atraso posterior cancela as pendências e suspende **todas** as novas ordens, inclusive fechamentos manuais, até a recuperação. Contas com posição aberta nesse instrumento também têm pendências canceladas e admissões suspensas nos demais instrumentos, pois seu patrimônio depende da referência indisponível. A última marcação é preservada para consulta, mas deve ser interpretada junto de `GetMarkPriceFeedStatus`: `Connected` indica a última consulta válida e `Fresh` indica validade temporal. A expiração é verificada antes de comandos e no ticker. Nenhum negócio já executado é revertido.

Parar o contexto mantém o vínculo suspenso. Reiniciar é permitido após o retorno da chamada anterior, com a mesma fonte, símbolo e escala, preservando o último horário aceito. Chamadas simultâneas são recusadas. Marcação manual e variação de liquidação manual são recusadas enquanto existir o vínculo, evitando referências concorrentes. Fechar o motor cancela requisições em andamento; o chamador deve aguardar o retorno do feed. O preço é específico do provedor escolhido e não representa um preço oficial da B3. O modelo continua em memória, sem roteamento externo, calendário de ajustes ou recuperação durável.

As premissas gerais de margem e variação são descritas pela [CME sobre marcação a mercado](https://www.cmegroup.com/education/courses/introduction-to-futures/mark-to-market) e pela [CME sobre margens inicial e de manutenção](https://www.cmegroup.com/solutions/risk-management/performance-bonds-margins/faq-performance-bonds-margins.html). Os parâmetros e a política implementados aqui são do simulador, sem promessa de conformidade integral com uma bolsa.

### Modelo de Concorrência

`SubmitOrder` copia a entrada antes de enfileirar o comando. O retorno confirma aceitação na fila, não execução; o objeto original não recebe alterações de status. Um ID já ativo no mesmo símbolo (no book ou em stops pendentes) é rejeitado no processamento; `OrderBook.AddOrder` ignora IDs já presentes e ordens cujo restante faria a quantidade total do nível exceder `uint64`. Isso protege o índice de cancelamento, sem oferecer deduplicação histórica de ordens concluídas. Cancelamentos e consultas aguardam o comando correspondente e observam os comandos anteriores na mesma fila. Chamadas simultâneas são ordenadas pelo recebimento no canal. O ticker de stops pertence ao próprio loop e executa os stops diretamente, sem enviar comandos ao próprio canal.

`GetOrderBook` retorna um book independente: suas mutações não alteram o motor e ele não acompanha alterações posteriores. `GetSnapshot` consulta o loop e devolve uma cópia profunda atualizada, inclusive após fills parciais. Books independentes protegem seus métodos públicos com um mutex; seus snapshots publicados usam `atomic.Pointer`, com cópia profunda na leitura. As consultas do motor não são lock-free nem zero-copy.

Trades executados aguardam publicação em uma fila interna. Ao atingir `max(1, tradeBufferSize)` trades pendentes nessa fila, novas submissões recebem backpressure. Esse valor é um limiar de admissão, não um limite rígido de memória: ordens já aceitas e stops podem produzir vários trades. O consumidor deve continuar drenando o canal. Cancelamentos e consultas continuam disponíveis durante o atraso.

Use `defer engine.Close()` para encerrar o loop e o monitor de cooldown do RiskManager. `Close` é idempotente e imediato: termina após o comando em execução, podendo descartar comandos aceitos ainda na fila e trades ainda não publicados; não é uma operação de flush. Trades já no buffer público continuam legíveis após o fechamento do canal. Aguarde os resultados e consuma os trades necessários antes de encerrar. Submissões após o fechamento retornam `ErrEngineClosed`; consultas retornam valores vazios e cancelamentos retornam `false`. Um RiskManager criado separadamente também deve receber `Close()`.

O RiskManager copia clientes, limites e circuit breakers na entrada; suas consultas devolvem cópias. Leituras de clientes e circuit breakers são protegidas durante a cópia, incluindo concorrência com atualizações de posições e cooldowns.

### Goroutines e virtual threads

Goroutines e virtual threads do Java 21 são tarefas leves gerenciadas pelo runtime, permitindo muitas tarefas concorrentes sem uma thread do sistema por tarefa. Nenhuma delas elimina corridas sobre objetos compartilhados. Em Go, canais fornecem comunicação e ordenação de memória; aqui, uma única goroutine mantém a posse do estado mutável. Virtual threads preservam a API `Thread` do Java e também exigem sincronização quando compartilham estado. Não há paralelismo de matching por ativo nesta etapa.

Referências oficiais: [Effective Go: concorrência](https://go.dev/doc/effective_go#concurrency), [modelo de memória do Go](https://go.dev/ref/mem), [JEP 444: virtual threads](https://openjdk.org/jeps/444).

## Instalação

### Pré-requisitos
- Go 1.27 ou posterior

### Setup
```bash
# Instalar dependências
go mod tidy

# Rodar o exemplo
go run .

# Rodar benchmarks
go test -bench=. -benchmem
```

## Uso

### Exemplo Básico

Exemplo para o `main.go` do pacote do projeto:

```go
package main

import (
    "fmt"

    "github.com/google/uuid"
)

func main() {
    engine := NewMatchingEngine(10000, 10000)
    defer engine.Close()

    order := &Order{
        ID:        uuid.New(),
        Symbol:    "BTC-USD",
        Side:      Bid,
        Type:      Limit,
        Price:     50000000,
        Quantity:  100,
        Remaining: 100,
    }
    if err := engine.SubmitOrder(order); err != nil {
        fmt.Printf("Order could not be queued: %v\n", err)
        return
    }
    fmt.Println("Order queued; risk checks and execution run in the matching loop")

    snapshot := engine.GetSnapshot(order.Symbol)
    if snapshot != nil && snapshot.BestBid != nil {
        fmt.Printf("Best bid: %d, remaining quantity: %d\n",
            snapshot.BestBid.Price, snapshot.BestBid.TotalQty)
    }
}
```

A demo interrompe o cenário quando uma submissão necessária falha. No cenário de carga, somente `ErrBackpressure` conta como rejeição por capacidade; erros de validação ou encerramento são reportados separadamente. As mensagens de risco distinguem entrada na fila de aprovação financeira, e as estatísticas são lidas após os comandos correspondentes.

### Leitura de Snapshot
```go
// Snapshot independente, consistente com os comandos anteriores
snapshot := engine.GetSnapshot("BTC-USD")
if snapshot != nil {
    if snapshot.BestBid != nil {
        fmt.Printf("Best Bid: $%.2f\n", float64(snapshot.BestBid.Price)/1_000_000)
    }
}
```

## Cenários de Teste (main.go)

A aplicação demo inclui 5 cenários:

1. **Submissão de Ordens Iniciais**: Submete 5 ordens de venda (asks)
2. **Matching de Ordens**: Submete ordem de compra que matchea com asks existentes
3. **Ordem de Mercado**: Testa execução de market order
4. **Cancelamento de Ordem**: Testa funcionalidade de cancelamento
5. **Backpressure**: Testa rejeição quando o canal está cheio

## Benchmarks

Execute benchmarks para medir performance:
```bash
go test -bench=. -benchmem
```

Nos benchmarks de carga, `ErrBackpressure` é uma recusa esperada; qualquer outro erro de submissão faz o benchmark falhar. A preparação dos books exige que todas as ordens sejam aceitas na fila. Os tempos medem tentativas de submissão, não garantem execução de todas as ordens.

### Benchmarks Disponíveis
- `BenchmarkMatchingEngine_SubmitOrder`: Throughput de submissão de ordens
- `BenchmarkOrderBook_GetSnapshot`: Latência de leitura de snapshots independentes
- `BenchmarkMatchingEngine_ConcurrentMatching`: Performance de matching concorrente
- `BenchmarkMatchingEngine_HighThroughput`: Stress test multi-worker

## Validação de Concorrência

```bash
go test ./...
go test -race ./...
go test -race -count=50 '-cpu=1,2,4' -shuffle=on -timeout=3m ./...
```

Os testes cobrem matching, stops e cancelamentos concorrentes, execução única, FIFO no mesmo preço, isolamento das entradas e consultas, acesso concorrente ao RiskManager e encerramento com consumidor lento. O detector de corridas requer CGO e um compilador C compatível.

## Estrutura do Projeto

```
Order Matching Engine/
├── go.mod              # Dependências do projeto
├── main.go             # Aplicação demo com 5 cenários
├── types.go            # Estruturas de dados core
├── orderbook.go        # Implementação do order book
├── matcher.go          # Motor de matching
├── benchmark_test.go   # Benchmarks de performance
└── README.md           # Documentação
```

## Evolução do Projeto

O [roadmap](ROADMAP.md) registra a relevância de mercado, lacunas atuais, decisões de domínio e critérios de conclusão para os próximos incrementos.

## Considerações de Produção

### Escalabilidade
- **Horizontal**: Deploy múltiplas instâncias do engine, shard por símbolo
- **Vertical**: Aumenta tamanhos de buffer para maior throughput
- **Persistência**: Adiciona persistência de ordens para crash recovery

### Monitoramento
- Track profundidade do order channel (indicador de backpressure)
- Monitorar lag de publicação de trades
- Medir profundidade do order book por símbolo

### Modos de Falha
- **Channel Full**: Rejeita ordens com erro explícito
- **Engine Crash**: Replay de ordens persistidas
- **Network Partition**: Circuit breaker na submissão de ordens

## Licença

MIT
