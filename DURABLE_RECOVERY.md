# Persistência e recuperação do motor de derivativos

## Abertura e retomada

`NewDurableDerivativesMatchingEngine(path, orderBufferSize, tradeBufferSize)` cria ou reabre um banco local bbolt. Ambos os buffers precisam ser positivos; o diretório do arquivo deve existir. Um segundo escritor no mesmo arquivo é recusado. O banco recupera books, ordem FIFO por preço, stops, posições, lotes, marcas, reservas, garantias, limites, circuit breakers, volumes diários, estatísticas, recibos idempotentes, política de liquidação e negócios não confirmados.

O construtor retorna `*DurableMatchingEngine`. Configuração, submissão e consultas usam os métodos desse perfil, que serializa alterações e consultas para impedir a exposição de um estado ainda não gravado. Ele não expõe um `RiskManager` mutável nem uma submissão sem recibo. Cadastros e aportes iniciais devem ser aplicados somente quando ainda não existirem; reaplicar uma garantia-base ao reabrir representa uma nova alteração financeira.

```go
engine, err := NewDurableDerivativesMatchingEngine("state.db", 512, 512)
if err != nil {
    return err
}
defer engine.Close()

handler, err := NewDurableOrderHTTPHandler(engine)
if err != nil {
    return err
}
// Montar handler em um servidor local com timeouts configurados.
_ = handler
```

O handler preserva o contrato HTTP existente: mesma chave e mesmos dados retornam o recibo original, inclusive após reinício, sem criar um novo negócio. O ID da ordem também continua reservado após cancelamento ou execução. Configurações e cancelamentos têm métodos próprios no perfil durável. Não há migração automática de um motor já em execução em memória para esse banco.

## Fronteira de confirmação

Cada alteração é processada sob exclusão mútua. O matching interno permanece com um único dono, sem publicação ou manutenção autônoma fora da transação do perfil durável. Depois da alteração, o snapshot financeiro, os recibos e os novos negócios são gravados juntos por uma transação bbolt com sincronização de escrita habilitada. Só depois do commit o recibo fica disponível ao chamador e os negócios podem ser consultados.

Reenvios que apenas consultam um recibo existente não criam outro checkpoint. A fila de admissões é limitada por `orderBufferSize`; escrita lenta pode produzir `ErrBackpressure`. Um contexto cancelado antes de iniciar não executa a solicitação. Depois de iniciada a operação, o perfil aguarda a resolução da gravação; se o contexto tiver sido cancelado, retorna resultado incerto e o cliente deve repetir a mesma chave e os mesmos dados.

Falha de serialização ou armazenamento interrompe o matching e bloqueia todas as novas alterações e consultas financeiras desse objeto. O erro inclui `ErrDurableStorage`; para submissões, também inclui `ErrRequestOutcomeUnknown`. Fechar e reabrir o banco permite verificar se o commit chegou a ocorrer: o reenvio recupera o recibo gravado ou processa a solicitação uma vez sobre o último estado confirmado. Não há tentativa de continuar sobre o estado candidato em memória após uma falha de escrita.

`Close` aguarda a operação em andamento, encerra a manutenção e fecha o banco. Ordens, stops e reservas já gravados continuam disponíveis na próxima abertura. O fechamento não gera cancelamentos financeiros no banco.

## Negócios pendentes e confirmação do consumidor

Este perfil oferece uma outbox consultável, em vez de expor o canal volátil do motor interno:

1. `GetTradeDeliveryState()` informa o último negócio confirmado, o último gerado e a quantidade pendente.
2. `ReadTrades(after, limit)` retorna cópias de até 10.000 eventos posteriores ao cursor, cada um com sequência e negócio de ID estável.
3. O consumidor aplica o negócio de forma idempotente, usando seu ID como identidade no destino.
4. `AcknowledgeTrades(sequence)` grava a confirmação cumulativa e libera os eventos até essa sequência. Repetir uma confirmação já gravada é inofensivo; confirmar uma sequência ainda inexistente é recusado.

Eventos lidos e ainda não confirmados reaparecem com os mesmos IDs e sequências após reinício. A entrega é **pelo menos uma vez**: uma queda entre aplicar no destino e confirmar na origem pode repetir a leitura. A confirmação local não é uma transação distribuída com o consumidor; o destino precisa deduplicar. Um cursor anterior à confirmação já gravada é recusado para tornar explícita a lacuna de retenção.

`tradeBufferSize` funciona como limiar de admissão da outbox. Ao atingir esse limiar, novas chaves são recusadas; recibos existentes ainda podem ser recuperados. A confirmação libera espaço. Uma ordem já admitida pode gerar vários fills e ultrapassar o limiar, como no contrato anterior de backpressure. Stops e liquidações automáticas aguardam capacidade quando a outbox está cheia. Nenhum evento é removido silenciosamente.

## Integridade, manutenção e limites

O formato atual é a versão 1: snapshot JSON determinístico, durações de política em nanossegundos e envelope com identificação de formato e SHA-256. Na abertura, são verificadas a estrutura do banco, a integridade do payload e a coerência entre ordens, reservas, posições, lotes, patrimônio, recibos e sequências da outbox. Dados inválidos ou versões não suportadas impedem a abertura com `ErrInvalidDurableState`; não são convertidos nem descartados automaticamente.

As cotações são recuperadas para contabilização, mas sua condição de referência fresca é invalidada. Uma marcação nova é exigida antes de retomar liquidações automáticas. Stops, cooldown e tentativas de liquidação são processados pela manutenção transacional desse perfil. O polling externo de preços do perfil em memória ainda não foi conectado ao perfil durável; `SetDerivativeMarkPrice` permite fornecer referências por transações explícitas.

Esta primeira implementação regrava um snapshot completo a cada alteração efetiva, com payload máximo de 64 MiB. O custo cresce com o estado e o histórico de recibos. Não é um log imutável de todos os comandos nem oferece migração automática, compactação, backup operacional, replicação ou alta disponibilidade. A recuperação foi testada com encerramento abrupto de processo, não como certificação de comportamento do hardware em perda de energia.

As medições anteriores de dez mil chamadas e p95 até 100 ms pertencem ao perfil em memória. Não devem ser atribuídas a este perfil, que sincroniza o disco. Medições específicas com armazenamento e evolução para registros incrementais permanecem como próximos passos.

## Validação

Os testes incluem processo separado encerrado antes e depois do commit; retorno de erro antes e depois de uma escrita efetiva; bloqueio de recibo e consultas enquanto o commit aguarda; admissão limitada; segundo escritor; checksum inválido; formato desconhecido; estados financeiros incoerentes; FIFO; aportes; cancelamentos; stops; cooldown; liquidação com referência renovada; repetição concorrente e confirmação de negócios.

```powershell
go test -run '^TestDurable' ./...
go test -race ./...
go test '-coverprofile=coverage.out' ./...
go tool cover '-func=coverage.out'
go vet ./...
```

Referência de implementação: [bbolt — transações e abertura do banco](https://github.com/etcd-io/bbolt).
