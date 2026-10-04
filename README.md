# Order Matching Engine

Um motor de matching de ordens de alta performance em Go, projetado para latência em microssegundos com tratamento robusto de concorrência.

## Funcionalidades

### Core Functionality
- **Price-Time Priority (FIFO)**: Ordens são matcheadas por preço primeiro, depois timestamp
- **Limit & Market Orders**: Suporte para ambos os tipos de ordens
- **Publicação de Trades em Tempo Real**: Trades publicados via canais bufferizados
- **Multi-Symbol Support**: Order books shardados por símbolo de trading

### Concorrência e Performance
- **Lock-Free Snapshots**: `atomic.Pointer[T]` para leituras zero-copy do topo do book
- **Locks Separados por Lado**: RWMutex independente para bids e asks para maximizar concorrência
- **Backpressure Handling**: Canais bufferizados com política de rejeição explícita
- **Object Pooling**: `sync.Pool` para zero-allocation no hot path
- **Sharded Order Books**: Map-based sharding por símbolo para escalabilidade horizontal

## Arquitetura

### Estruturas de Dados
- **Order**: Representa uma ordem de trading com ID, símbolo, lado, tipo, preço, quantidade
- **OrderBook**: Mantém bids (max-heap) e asks (min-heap) para um único símbolo
- **PriceLevel**: Agrupa ordens no mesmo preço com ordenação FIFO
- **Trade**: Representa um trade matcheado com detalhes de execução

### Modelo de Concorrência
```
┌─────────────────┐
│  Order Channel  │ (bufferizado, backpressure)
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ Matching Loop   │ (single goroutine por engine)
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│  Order Books    │ (shardados por símbolo)
│  ┌───────────┐  │
│  │ Bids (RWM)│  │
│  │ Asks (RWM)│  │
│  └───────────┘  │
└────────┬────────┘
         │
         ▼
┌─────────────────┐
│ Trade Channel   │ (bufferizado)
└─────────────────┘
```

### Lock-Free Snapshots
Snapshots do topo do book usam `atomic.Pointer[OrderBookSnapshot]` para leituras lock-free:
- **Writes**: Atualiza snapshot após cada modificação de ordem
- **Reads**: Single atomic load, sem bloqueio
- **Memory**: Deep copy garante isolamento do snapshot

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

### Leitura de Snapshot Lock-Free
```go
// Leitura de snapshot zero-cost (sem locks)
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
- `BenchmarkOrderBook_GetSnapshot`: Latência de leitura lock-free
- `BenchmarkMatchingEngine_ConcurrentMatching`: Performance de matching concorrente
- `BenchmarkMatchingEngine_HighThroughput`: Stress test multi-worker

## Desafios de Concorrência Endereçados

### 1. Leitura/Escrita Concorrente
**Solução**: RWMutex separado para bids e asks
- Leituras de cotação (best bid/ask) não bloqueiam matching
- Matching só trava o lado sendo modificado
- Permite leituras concorrentes em ambos os lados

### 2. Visibilidade de Memória
**Solução**: `atomic.Pointer[T]` para snapshots
- Garante visibilidade imediata across goroutines
- Sem polling necessário
- Single atomic load para leituras

### 3. Lock-Free vs Locked
**Solução**: Abordagem híbrida
- Hot path (snapshots): Operações atômicas lock-free
- Operações complexas (matching): Mutex para corretude
- Evita loops CAS e potencial livelock

### 4. Backpressure
**Solução**: Canais bufferizados com política explícita
- Order channel: Retorna `ErrBackpressure` quando cheio
- Trade channel: Bloqueia brevemente para evitar perda de dados
- Tamanhos de buffer configuráveis por deployment

### 5. Zero-Allocation
**Solução**: `sync.Pool` para objetos de ordem
- Reusa estruturas de ordem
- Reduz pressão do GC
- Crítico para latência em microssegundos

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
