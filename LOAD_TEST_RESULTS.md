# Ensaio local de submissão HTTP e idempotência

Data: 25/10/2026. Ambiente: Windows/amd64, Go 1.27.1, Intel Core i5-10210U, `GOMAXPROCS=8`. Cliente e servidor HTTP executados no mesmo processo e host, por loopback, sem TLS. Medições abaixo sem detector de corridas ou instrumentação de cobertura.

## Carga e meta

- 10.000 chamadas HTTP no total, com 64 trabalhadores e conexões reutilizadas; não é um gerador de 10.000 requisições por segundo em regime sustentado.
- 2.500 ordens distintas, cada uma enviada quatro vezes: 7.500 reenvios, mantendo chave e conteúdo.
- Duas contas de derivativos, um instrumento, preço inteiro fixo, contratos unitários, compras e vendas em quantidades iguais. Cada conta começa com garantia de 1.000.000 unidades e margem inicial de 100 por contrato.
- Consumidor de trades ativo durante o ensaio. A fila do motor e o canal de trades têm capacidade de 512 cada.
- Meta experimental: latência HTTP p95 até **100 ms**, medida desde o envio até a leitura completa da resposta. A latência inclui comunicação local e processamento da aplicação; a espera do trabalho antes do envio não entra no percentil. A duração total e a taxa média incluem o despacho dos trabalhos.

## Três execuções consecutivas

| Execução | Duração das chamadas | Taxa média observada | p50 | p95 | p99 | Meta p95 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 690 ms | 14.500 chamadas/s | 2,021 ms | 12,178 ms | 22,678 ms | Atingida |
| 2 | 722 ms | 13.845 chamadas/s | 2,054 ms | 10,258 ms | 17,374 ms | Atingida |
| 3 | 844 ms | 11.854 chamadas/s | 2,065 ms | 14,820 ms | 26,515 ms | Atingida |

As taxas são médias de lotes curtos. Não demonstram capacidade sustentada, distribuição entre máquinas, desempenho com TLS, persistência, autenticação ou latência de mercado. Não foram definidos SLAs de produção nem metas p99 nesta etapa.

## Invariantes verificados em cada execução

- Zero falhas HTTP; 2.500 recibos originais e 7.500 replays com corpo idêntico ao original de cada chave.
- Exatamente 2.500 ordens processadas e 1.250 negócios publicados, com IDs de negócios únicos, preço 100 e quantidade unitária.
- Posições finais de +1.250 e -1.250 contratos, sem ordens residuais no book ou reservas de ordens pendentes.
- Garantia/patrimônio de 1.000.000 em cada conta, margem de posição de 125.000 e garantia disponível de 875.000. P&L nulo neste cenário de preço fixo.

Os testes adicionais cobrem reenvio concorrente da mesma chave, replay após fill e cancelamento, conflito de conteúdo, ID reutilizado com nova chave, rejeição financeira que continua rejeitada após aporte, capacidade do histórico, fila cheia, backlog de publicação e resultado incerto após cancelamento ou desligamento. O parser HTTP rejeita JSON ambíguo, duplicado e fora do contrato.

## Reprodução e validações

```powershell
go test -run '^TestHTTPOrderLoad10000' -count=3 -v ./...
go test '-coverprofile=coverage.out' ./...
go tool cover '-func=coverage.out'
go test -race ./...
go vet ./...
```

O detector de corridas exige CGO e compilador C configurados no ambiente. A suíte completa, incluindo a carga HTTP, passou com o detector e cobertura total de **95,6%**, sem excluir a demo. Os cenários de concorrência e repetição também passaram em dez repetições para cada uma de 1, 2 e 4 CPUs. Todos os benchmarks existentes passaram em execução curta de dez iterações; isso não é uma comparação estatística de desempenho.

Este ensaio usa o histórico idempotente em memória, limitado por capacidade. O perfil de [recuperação durável](DURABLE_RECOVERY.md), acrescentado em incremento posterior, precisa de medições próprias e não herda os números desta tabela. Histórico imutável de comandos, autenticação e implantação de um servidor persistente continuam pendentes. A carga valida a integração do handler HTTP com o matching e o risco do simulador.
