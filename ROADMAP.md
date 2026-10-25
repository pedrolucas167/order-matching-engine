# Roadmap de evolução do Order Matching Engine

## Objetivo e relevância

Evoluir o núcleo atual para um simulador de execução com regras financeiras explícitas e verificáveis. Livro de ofertas, melhor preço, fila por preço, execução parcial e cancelamentos correspondem a mecanismos usados em mercados reais. A implementação atual tem utilidade como núcleo de simulação e estudo de infraestrutura; ainda não é uma plataforma pronta para operar recursos reais nem comprova demanda comercial.

As regras oficiais da [Coinbase](https://www.coinbase.com/legal/trading_rules) descrevem prioridade de preço/exibição/tempo, reserva de ativos por ordem e stops referenciados no último negócio. A [CME](https://www.cmegroup.com/education/matching-algorithm-overview) utiliza diferentes algoritmos, incluindo FIFO e pro rata. A [B3](https://www.b3.com.br/pt_br/solucoes/plataformas/puma-trading-system/para-participantes-e-traders/ferramentas-para-traders/risco-pre-negociacao-line/) mantém controles de risco anteriores ao núcleo de negociação. Essas referências orientam a comparação; este projeto não implementa integralmente as regras de nenhuma dessas instituições.

## Entregas concluídas

- Matching com um único dono mutável, cópias independentes de entradas e consultas, cancelamentos serializados e execução de stops no mesmo loop.
- Regressões para preço/FIFO, fills parciais, IDs ativos duplicados, quantidades, estados finais e referência de preço sem overflow.
- Validação estrutural antes da fila, erros de submissão tratados nos chamadores e distinção entre entrada na fila e aprovação pelo risco.
- Testes com detector de corridas, repetições concorrentes e execução curta de todos os benchmarks. Esses resultados não substituem validação financeira nem demonstram capacidade ou latência de produção.

A [correção de concorrência](https://github.com/pedrolucas167/order-matching-engine/pull/1) e a [validação de entrada](https://github.com/pedrolucas167/order-matching-engine/pull/2) estão em pull requests abertas, ainda sem merge. A segunda depende da primeira.

## Decisão de domínio antes do primeiro incremento

**Pendente de definição com o usuário:** mercado à vista, derivativos com margem ou apenas matching sem contabilidade financeira.

A proposta inicial é simular mercado à vista, sem alavancagem: uma compra compromete saldo na moeda de cotação; uma venda compromete estoque do ativo. Derivativos exigem outro tratamento de posições vendidas, garantias e margem. Essa escolha deve preceder mudanças no modelo financeiro.

## Etapas e critérios de conclusão

| Etapa | Resultado esperado | Critério de conclusão |
| --- | --- | --- |
| 1. Modelo financeiro e reservas | Contas por cliente/ativo, unidades explícitas, aritmética verificada, reservas e limites coerentes com o mercado escolhido | Ordens simultâneas não reutilizam saldo ou estoque; fills transferem os ativos corretos; cancelamentos e rejeições liberam somente a reserva restante; posições respeitam o instrumento e os limites definidos |
| 2. Registro durável e recuperação | Eventos sequenciados, contrato de aceitação durável, replay e tratamento de retransmissões | Reiniciar a partir do registro reconstrói os mesmos books, posições e saldos; retransmitir uma solicitação não duplica execução; perda de publicação e interrupção têm comportamento verificável |
| 3. Regras do mercado escolhido | Tamanho de lote e tick, instruções de validade, prevenção de autonegociação e regras explícitas para stops e interrupções | Cenários de conformidade passam para as regras selecionadas; prioridade, cancelamentos e disparos não dependem de convenções implícitas |
| 4. Interface de execução e observabilidade | Resultados por ordem, identificadores de eventos, consultas, dados de mercado sequenciados e autenticação quando houver acesso externo | Um consumidor distingue validação, aceitação, rejeição pelo risco, fill parcial, fill total e cancelamento; reconexão detecta lacunas de eventos |
| 5. Simulação e medições representativas | Replay de dados, cargas e falhas controladas, métricas de latência e uso de recursos | Os resultados são reproduzíveis, os invariantes financeiros são preservados e as medições declaram carga, máquina, distribuição e limites do experimento |

Cada incremento deve ter branch própria, commits pequenos em Conventional Commits, sem coautoria, testes pertinentes e uma descrição curta de PR. Não fazer merge automático. As etapas posteriores não autorizam, por si, implantação, integração a uma bolsa, inclusão de CI ou uso de dinheiro real.

## Primeiro incremento financeiro proposto

Após definir o domínio, limitar a primeira implementação à aceitação e ao ciclo de vida de reservas:

1. Definir ativos/unidades e o estado financeiro por cliente, sem somar quantidades de instrumentos diferentes em uma única posição.
2. Verificar operações aritméticas e limites antes de comprometer recursos.
3. Reservar recursos quando a ordem for aprovada pelo risco, no caminho serializado do matching.
4. Atualizar a reserva proporcionalmente a cada fill; devolver excesso quando houver melhora de preço, cancelamento ou rejeição do restante.
5. Cobrir reservas concorrentes, fills parciais, cancelamentos, rejeições, isolamento de consultas e conservação de saldos.

Compras market e stops precisam de uma política explícita de orçamento ou proteção de preço. O preço opcional fornecido em uma ordem market não deve ser tratado como custo garantido de execução. Taxas e alavancagem não devem ser presumidas como implementadas.

## Lacunas concretas encontradas no código atual

- `checkCollateral` compara uma ordem isolada com a garantia, mas não reserva recursos para ordens abertas. Duas ordens podem passar individualmente e comprometer mais do que o saldo total.
- `Exposure` usa um inteiro sem sinal e é reduzido em vendas. Uma exposição que deveria ficar negativa pode sofrer underflow; a correção depende de definir venda de estoque versus abertura de posição vendida.
- `LongPosition` e `ShortPosition` são agregadas por cliente, sem dimensão por instrumento. O modelo ainda não representa adequadamente uma carteira com ativos diferentes.
- `MaxPositionSize` está declarado, mas não é aplicado nas verificações de risco.
- Stops usam o ponto médio das melhores ofertas, ou a única oferta disponível, verificado periodicamente. Isso difere, por exemplo, da referência ao último negócio definida nas regras citadas da Coinbase.
- Estado e publicação são mantidos em memória. `Close` continua imediato, sem flush, e pode descartar comandos e trades pendentes conforme o contrato atual.

A etapa 1 deve corrigir o modelo financeiro selecionado, sem reescrever simultaneamente matching, persistência e interfaces. Recuperação durável, mudanças na referência de stops e novos protocolos ficam para os incrementos correspondentes.
