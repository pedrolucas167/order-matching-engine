# Order Matching Engine

Um motor de matching de ordens de alta performance em Go, projetado para latência em microssegundos com tratamento robusto de concorrência.

## Funcionalidades

### Core Functionality
- **Price-Time Priority (FIFO)**: Ordens são matcheadas por preço primeiro, depois timestamp
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

### Modelo de Concorrência

`SubmitOrder` copia a entrada antes de enfileirar o comando. O retorno confirma aceitação na fila, não execução; o objeto original não recebe alterações de status. Cancelamentos e consultas aguardam o comando correspondente e observam os comandos anteriores na mesma fila. Chamadas simultâneas são ordenadas pelo recebimento no canal. O ticker de stops pertence ao próprio loop e executa os stops diretamente, sem enviar comandos ao próprio canal.

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
go run main.go

# Rodar benchmarks
go test -bench=. -benchmem
```

## Uso

### Exemplo Básico
```go
package main

import (
    "awesomeProject"
    "github.com/google/uuid"
    "time"
)

func main() {
    // Cria engine com buffer de 10K ordens e 10K trades
    engine := awesomeProject.NewMatchingEngine(10000, 10000)
    defer engine.Close()

    // Submete uma ordem limit
    order := &awesomeProject.Order{
        ID:        uuid.New(),
        Symbol:    "BTC-USD",
        Side:      awesomeProject.Bid,
        Type:      awesomeProject.Limit,
        Price:     50000000,  // $50.00 em micros
        Quantity:  100,
        Remaining: 100,
        Timestamp: time.Now().UnixNano(),
    }
    engine.SubmitOrder(order)

    // Consome trades
    for trade := range engine.GetTradeChannel() {
        fmt.Printf("Trade: %s @ $%.2f\n", trade.Symbol, float64(trade.Price)/1_000_000)
    }
}
```

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

### Benchmarks Disponíveis
- `BenchmarkMatchingEngine_SubmitOrder`: Throughput de submissão de ordens
- `BenchmarkOrderBook_GetSnapshot`: Latência de leitura de snapshots independentes
- `BenchmarkMatchingEngine_ConcurrentMatching`: Performance de matching concorrente
- `BenchmarkMatchingEngine_HighThroughput`: Stress test multi-worker

## Validação de Concorrência

```bash
go test ./...
go test -race ./...
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
