# Roadmap de evolução do Order Matching Engine

## Objetivo e relevância

Evoluir o núcleo atual para um simulador de execução com regras financeiras explícitas e verificáveis. Livro de ofertas, melhor preço, fila por preço, execução parcial e cancelamentos correspondem a mecanismos usados em mercados reais. A implementação atual tem utilidade como núcleo de simulação e estudo de infraestrutura; ainda não é uma plataforma pronta para operar recursos reais nem comprova demanda comercial.

As regras oficiais da [Coinbase](https://www.coinbase.com/legal/trading_rules) descrevem prioridade de preço/exibição/tempo, reserva de ativos por ordem e stops referenciados no último negócio. A [CME](https://www.cmegroup.com/education/matching-algorithm-overview) utiliza diferentes algoritmos, incluindo FIFO e pro rata. A [B3](https://www.b3.com.br/pt_br/solucoes/plataformas/puma-trading-system/para-participantes-e-traders/ferramentas-para-traders/risco-pre-negociacao-line/) mantém controles de risco anteriores ao núcleo de negociação. Essas referências orientam a comparação; este projeto não implementa integralmente as regras de nenhuma dessas instituições.

## Entregas concluídas

- Matching com um único dono mutável, cópias independentes de entradas e consultas, cancelamentos serializados e execução de stops no mesmo loop.
- Regressões para preço/FIFO, fills parciais, IDs ativos duplicados, quantidades, estados finais e referência de preço sem overflow.
- Validação estrutural antes da fila, erros de submissão tratados nos chamadores e distinção entre entrada na fila e aprovação pelo risco.
- Testes com detector de corridas, repetições concorrentes e execução curta de todos os benchmarks. Esses resultados não substituem validação financeira nem demonstram capacidade ou latência de produção.

A [correção de concorrência](https://github.com/pedrolucas167/order-matching-engine/pull/1) e a [validação de entrada](https://github.com/pedrolucas167/order-matching-engine/pull/2) foram entregues em pull requests encadeadas. O estado atualizado de revisão e merge deve ser consultado nesses links.

## Domínio escolhido

**Confirmado pelo usuário:** derivativos com margem e posições compradas ou vendidas por instrumento.

O primeiro incremento usa um perfil explícito de derivativos, com margem inicial fixa por contrato, denominada na mesma moeda da garantia da conta. Compradores e vendedores podem abrir posições sem pagar ou receber o valor nocional integral. O modelo é uma simplificação para simulação, não um cálculo de margem de bolsa.

Referências: [margem inicial e de manutenção na CME](https://www.cmegroup.com/solutions/risk-management/performance-bonds-margins/faq-performance-bonds-margins.html) e [finalidade das margens em futuros — CFTC](https://www.cftc.gov/LearnAndProtect/AdvisoriesAndArticles/economicpurpose.html).

A etapa 1 fica dividida em **1a: posições, margem inicial e reservas** e **1b: resultado financeiro, ajuste por preço, margem de manutenção e resposta à insuficiência de margem**. A etapa 1a está implementada. A etapa 1b agora inclui multiplicador, lotes FIFO, P&L realizado/não realizado, marcação explícita, variação de liquidação manual, margem de manutenção e bloqueio de novo risco com cancelamento de pendências. Ainda não há SPAN, compensação entre produtos, alimentação automática de preços ou liquidação automática.

## Etapas e critérios de conclusão

| Etapa | Resultado esperado | Critério de conclusão |
| --- | --- | --- |
| 1. Modelo financeiro e reservas | Contas por cliente/ativo, unidades explícitas, aritmética verificada, reservas e limites coerentes com o mercado escolhido | Ordens simultâneas não reutilizam garantia; fills atualizam posições líquidas por instrumento; cancelamentos e rejeições liberam somente a reserva restante, preservando a margem de posições ainda abertas |
| 2. Registro durável e recuperação | Eventos sequenciados, contrato de aceitação durável, replay e tratamento de retransmissões | Reiniciar a partir do registro reconstrói os mesmos books, posições e saldos; retransmitir uma solicitação não duplica execução; perda de publicação e interrupção têm comportamento verificável |
| 3. Regras do mercado escolhido | Tamanho de lote e tick, instruções de validade, prevenção de autonegociação e regras explícitas para stops e interrupções | Cenários de conformidade passam para as regras selecionadas; prioridade, cancelamentos e disparos não dependem de convenções implícitas |
| 4. Interface de execução e observabilidade | Resultados por ordem, identificadores de eventos, consultas, dados de mercado sequenciados e autenticação quando houver acesso externo | Um consumidor distingue validação, aceitação, rejeição pelo risco, fill parcial, fill total e cancelamento; reconexão detecta lacunas de eventos |
| 5. Simulação e medições representativas | Replay de dados, cargas e falhas controladas, métricas de latência e uso de recursos | Os resultados são reproduzíveis, os invariantes financeiros são preservados e as medições declaram carga, máquina, distribuição e limites do experimento |

Cada incremento deve ter branch própria, commits pequenos em Conventional Commits, sem coautoria, testes pertinentes e uma descrição curta de PR. Não fazer merge automático. As etapas posteriores não autorizam, por si, implantação, integração a uma bolsa, inclusão de CI ou uso de dinheiro real.

## Incremento 1a implementado nesta branch

No domínio escolhido, limitar o primeiro incremento ao ciclo de vida da margem inicial e das reservas:

1. Registrar instrumentos, moeda de garantia e margem por contrato; manter posições líquidas com sinal por cliente e instrumento.
2. Verificar operações aritméticas e limites antes de comprometer recursos.
3. Reservar recursos quando a ordem for aprovada pelo risco, no caminho serializado do matching.
4. Converter margem de ordens em margem de posição a cada fill; liberar somente a reserva remanescente em cancelamentos e rejeições, e liberar margem de posição quando a posição for encerrada.
5. Cobrir reservas concorrentes, fills parciais, cancelamentos, rejeições, isolamento de consultas, contraposição dos contratos e invariantes de garantia disponível e comprometida.

Neste incremento, a margem de ordens limit, market e stop depende da quantidade de contratos, não do preço informado. Stops já comprometem margem enquanto estão pendentes. Para cada instrumento, o orçamento cobre a maior posição líquida alcançável se as compras ou vendas em aberto executarem. Não há compensação de margem entre instrumentos diferentes. Proteção de preço, taxas e atualização dinâmica da margem ficam fora deste incremento.

O perfil financeiro é criado explicitamente por `NewDerivativesMatchingEngine`. `NewMatchingEngine` permanece como simulação educativa legada; os perfis não são misturados no mesmo motor. O primeiro perfil exige instrumentos e clientes cadastrados, posições com sinal e moeda de garantia compatível.

A implementação reserva margem no mesmo fluxo serializado das ordens, liquida a quantidade das duas contrapartes antes de publicar o trade e libera reservas do restante em cancelamentos, rejeições e encerramento do motor. Fechar o motor não encerra posições: a margem de contratos já executados permanece comprometida. Esse comportamento continua sem flush ou recuperação durável.

A etapa **1b**, detalhada abaixo, acrescenta multiplicador, marcação, resultado financeiro, variação de liquidação manual e controles de margem. Liquidação automática, taxas, proteção de preço e dados externos continuam precisando de contratos próprios antes de considerar o modelo uma gestão completa de risco.

## Lacunas identificadas no modelo anterior

- `checkCollateral` compara uma ordem isolada com a garantia, mas não reserva recursos para ordens abertas. Duas ordens podem passar individualmente e comprometer mais do que o saldo total.
- `Exposure` usa um inteiro sem sinal e é reduzido em vendas. Uma exposição que deveria ficar negativa pode sofrer underflow; a correção depende de definir venda de estoque versus abertura de posição vendida.
- `LongPosition` e `ShortPosition` são agregadas por cliente, sem dimensão por instrumento. O modelo ainda não representa adequadamente uma carteira com ativos diferentes.
- `MaxPositionSize` está declarado, mas não é aplicado nas verificações de risco.
- Stops usam o ponto médio das melhores ofertas, ou a única oferta disponível, verificado periodicamente. Isso difere, por exemplo, da referência ao último negócio definida nas regras citadas da Coinbase.
- Estado e publicação são mantidos em memória. `Close` continua imediato, sem flush, e pode descartar comandos e trades pendentes conforme o contrato atual.

A etapa 1 deve corrigir o modelo financeiro selecionado, sem reescrever simultaneamente matching, persistência e interfaces. Recuperação durável, mudanças na referência de stops e novos protocolos ficam para os incrementos correspondentes.

No perfil novo, a reserva, as posições por instrumento e o limite de posição potencial endereçam as quatro primeiras lacunas acima. O modelo legado continua educativo e não deve ser utilizado como contabilidade financeira de derivativos. Referência dos stops e recuperação durável permanecem nas etapas futuras; não foram alteradas silenciosamente neste incremento.

## Incremento 1b implementado nesta branch

- Lotes FIFO mantêm custo de entrada exato, inclusive em fills parciais e reversões; os resultados têm sinal.
- Marcação e variação de liquidação são comandos explícitos do mesmo dono do matching. Repetir uma liquidação no mesmo preço não duplica P&L.
- Patrimônio incorpora aportes e resultados realizados/não realizados; déficits inicial e de manutenção são consultáveis mesmo quando o patrimônio é negativo.
- Contas com déficit inicial têm ordens pendentes canceladas e podem reduzir posições por market, sem criar ou inverter exposição. Reforços de aporte são permitidos; retiradas que deixem déficit são recusadas.
- Atualizações com overflow são atômicas, e somas de P&L não dependem da ordem de iteração dos instrumentos.

O próximo passo financeiro é definir liquidação e proteção de preço antes de automatizá-las. Também permanecem pendentes calendário, feed de marcação, custódia/saques, taxas e integração a uma clearing. A variação implementada é uma simulação contábil manual, não transferência bancária nem homologação de mercado.
