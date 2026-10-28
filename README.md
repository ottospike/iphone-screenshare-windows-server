# iPhone Screenshare Windows Server

Recebe o **espelhamento de tela do iPhone (AirPlay)** no Windows pelo Wi-Fi, numa janela que o
**OBS** captura. É um único `.exe`: sem instalador, sem Bonjour, sem conta, sem trial.

Por baixo roda o [UxPlay](https://github.com/FDH2/UxPlay) (receptor AirPlay open source) com o
mDNS embutido dele, então o iPhone acha o PC sem precisar do Bonjour da Apple. O app fica na bandeja
do Windows, mantém o UxPlay no ar e reinicia se ele cair.

Vários iPhones ao mesmo tempo, cada um no seu **slot**: o iPhone que conectou no slot 1 fica preso
a ele e, se cair e reconectar, volta para o slot 1, na mesma fonte do OBS.

## Download

Baixe o `iPhoneScreenshareServer.exe` da [última release](../../releases/latest).
Requisitos: Windows 10 ou 11 (64 bits), iPhone e PC **na mesma rede**.

## Uso

1. Abra o `iPhoneScreenshareServer.exe`. Na primeira vez:
   - ele extrai o UxPlay para `%LOCALAPPDATA%\iPhoneScreenshareServer\` (alguns segundos);
   - o Windows pede permissão de administrador **uma vez**, para liberar o firewall.
2. No iPhone: **Central de Controle → Espelhar Tela → LIVE 1** (o segundo iPhone: **LIVE 2**).
3. No OBS, uma fonte por slot: **Fontes → + → Captura de Janela**
   - Janela: `[uxplay.exe]: Slot 1` (e `Slot 2` na outra fonte). A janela só existe enquanto o
     iPhone espelha: conecte o iPhone uma vez para ela aparecer na lista
   - Método de captura: **Windows 10 (1903 e superior)**
   - Prioridade de correspondência: **O título da janela deve corresponder**. A janela fecha quando
     o iPhone desconecta e volta com o mesmo título quando ele reconecta; assim cada fonte acha o
     seu slot e nunca troca de iPhone.

## Slots: cada iPhone no seu lugar

Com `"slots": 2` (o padrão), aparecem **LIVE 1** e **LIVE 2** no Espelhar Tela, cada um com a sua
janela (`Slot 1`, `Slot 2`) no OBS.

- O **primeiro iPhone** que conecta num slot livre fica **vinculado** a ele, pelo `deviceID` que o
  iPhone manda na conexão AirPlay. O vínculo fica salvo no config e sobrevive a reiniciar o app e o PC.
- Depois do vínculo, o slot **só aceita aquele iPhone** e os outros slots **recusam** ele. Se o
  iPhone do slot 1 tocar em LIVE 2 por engano, não entra.
- As travas passam a valer quando o slot fica sem ninguém espelhando (o app reinicia o UxPlay
  daquele slot sozinho, em segundo plano), para não derrubar quem está no ar.
- Para trocar o iPhone de um slot: menu → `Slot N` → **Desvincular iPhone deste slot**. O próximo
  que conectar fica com ele.
- Com `"slots": 1`, o nome é só **LIVE** e não há vínculo: qualquer iPhone entra.

## Bandeja (o painel de controle)

Clique no ícone da bandeja (perto do relógio):

| Item | O que faz |
|---|---|
| 1ª linha | Qualidade atual e número de slots |
| `Slot 1 "LIVE 1": <iPhone>, <estado>` | Quem está vinculado e o estado (`esperando o iPhone`, `conectado: <iPhone>`, `reconectando`, erro). Submenu: **Desvincular** |
| **FHD 1080p / HD 720p** | Qualidade, para todos os slots. A escolha fica salva. Trocar reinicia o UxPlay: espelhe de novo no iPhone |
| Reiniciar espelho | Relê o config e reinicia o UxPlay de todos os slots |
| Abrir log | Log do app e do UxPlay no Bloco de Notas |
| Abrir config | `config.json` no Bloco de Notas |
| Sair | Fecha o app e o UxPlay |

A cor do ícone é a do pior slot: **verde** no ar, **amarelo** reconectando, **vermelho** erro,
**cinza** iniciando.

## Quando a conexão cai

| O que acontece | O que o app faz |
|---|---|
| Wi-Fi do iPhone pisca | Espelhe de novo no iPhone: entra na hora (`-nohold`), sem esperar a sessão antiga expirar |
| iPhone some sem avisar | O UxPlay solta a sessão depois de 15 s sem resposta e a janela fica congelada no último quadro |
| UxPlay fecha ou trava e sai | Sobe de novo em 2 s. Se cair 3 vezes seguidas logo ao abrir, para e avisa (para não ficar em loop): veja o log e use **Reiniciar espelho** |
| Rede do PC muda (cabo/Wi-Fi volta, IP novo) | Reinicia o UxPlay para ele se anunciar na rede nova |
| PC volta da suspensão | Reinicia o UxPlay pelo mesmo motivo |
| App aberto duas vezes | A segunda cópia avisa e fecha |

O que **nenhum** receptor consegue: fazer o iPhone voltar a espelhar sozinho. Quando o espelhamento
cai, o iOS encerra do lado dele; é preciso tocar em **Espelhar Tela → LIVE N** de novo. Ele volta
para o mesmo slot e a mesma fonte do OBS.

## Config

`%LOCALAPPDATA%\iPhoneScreenshareServer\config.json` (menu → Abrir config). Depois de editar, use
**Reiniciar espelho**; para mudar `slots`, feche e abra o app.

```json
{
  "nome": "LIVE",
  "slots": 2,
  "qualidade": "FHD",
  "uxplay": "",
  "argsExtras": [],
  "vinculos": {
    "1": { "deviceID": "AA:BB:CC:DD:EE:FF", "iphone": "iPhone de Fulano" }
  }
}
```

| Campo | Para quê |
|---|---|
| `nome` | Nome que aparece em **Espelhar Tela** no iPhone (com 2+ slots: `LIVE 1`, `LIVE 2`...) |
| `slots` | Quantos iPhones ao mesmo tempo, de 1 a 4 |
| `vinculos` | Que iPhone está preso a cada slot. O app preenche sozinho; apagar uma entrada = desvincular |
| `qualidade` | `FHD` (1920x1080) ou `HD` (1280x720), ambos a 60 fps |
| `uxplay` | Caminho de outro `uxplay.exe`. Vazio = o embutido |
| `argsExtras` | Opções a mais do UxPlay, ex.: `["-reset", "30"]` (tolerar 30 s sem resposta antes de soltar a sessão) ou `["-pin", "1234"]` |

Em cada slot, quem conectar por último fica com a tela (`-nohold`); como o slot vinculado só aceita
o seu iPhone, isso só serve para a reconexão dele depois de uma queda ser imediata.

## Dicas para a live

- PC no **cabo de rede**; iPhone no Wi-Fi de **5 GHz** do mesmo roteador.
- Se outro iPhone for a câmera (Iriun etc.), ligue esse pelo **USB** para não dividir o Wi-Fi.
- No iPhone espelhado: **Não Perturbe** ligado e bloqueio automático desligado.
- O AirPlay tem uns 100–200 ms de atraso; se o áudio vier dele, ajuste a sincronia no OBS
  (Propriedades avançadas de áudio).

## Problemas comuns

| Sintoma | O que fazer |
|---|---|
| "LIVE" não aparece no iPhone | Mesma rede? Rede de convidados e "isolamento de clientes" do roteador bloqueiam. Rede do Windows como **Privada** (a regra de firewall vale para Privada/Domínio; o app avisa ao abrir se ela estiver Pública) |
| Aparece mas não conecta | Firewall: confira a regra "iPhone Screenshare Windows Server" em *Firewall do Windows → Regras de entrada*. Reabra o app |
| Tela preta no OBS | Troque o método de captura da Captura de Janela, ou use Captura de Tela com corte |
| Ícone vermelho | Menu → Abrir log; a última linha diz o motivo |
| Antivírus reclama | Libere a pasta `%LOCALAPPDATA%\iPhoneScreenshareServer\` |

## Como funciona

```
iPhoneScreenshareServer.exe (Go, bandeja)
 ├─ bundle/uxplay.zip embutido ── extraído 1x ──> %LOCALAPPDATA%\iPhoneScreenshareServer\uxplay-<hash>\
 │                                                 bin\uxplay.exe + DLLs, plugins do GStreamer
 ├─ por slot: uxplay.exe -n "LIVE N" -nh -nohold -p <portas do slot> -s 1920x1080 -fps 60
 │            + vinculado: -restrict -allow <deviceID>; os outros slots: -block <deviceID>
 ├─ lê a saída do UxPlay: "connection request from <iPhone> ... deviceID = <id>" → vincula o slot
 ├─ renomeia a janela de vídeo de cada UxPlay para "Slot N" (o OBS acha a fonte pelo título)
 ├─ vigia o processo: caiu → sobe de novo; 3 quedas rápidas → para e avisa
 ├─ vigia a rede: IP mudou ou PC voltou da suspensão → reinicia o UxPlay
 └─ job object do Windows: se o app fechar de qualquer jeito, o uxplay fecha junto
```

O UxPlay e o GStreamer são compilados no GitHub Actions (Windows + MSYS2 UCRT64) a partir de um
commit fixo do [FDH2/UxPlay](https://github.com/FDH2/UxPlay) (≥ 1.74, que traz o mDNS próprio),
com um patch de uma linha: saída sem buffer, para o app ver na hora qual iPhone conectou.
O CI também testa o resultado num Windows limpo: todos os elementos do GStreamer presentes, o
`uxplay.exe` sobe sozinho, e o `iPhoneScreenshareServer.exe` extrai e mantém o UxPlay no ar.

## Build

O `.exe` sai do workflow [`build`](.github/workflows/build.yml):

- todo push na `main` gera o artifact `iPhoneScreenshareServer`;
- uma tag `v*` publica a release.

Para atualizar o UxPlay, troque `UXPLAY_REF` no workflow pelo commit novo.

Local (precisa do `bundle/uxplay.zip` gerado pelo CI):

```
GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -s -w" -o dist/iPhoneScreenshareServer.exe .
```

| Arquivo | Papel |
|---|---|
| `main.go` | App de bandeja |
| `ci/montar-bundle.sh` | Compila o UxPlay e monta o `bundle/uxplay.zip` (no MSYS2) |
| `ci/testar.ps1` | Teste de fumaça no Windows do CI |

## Licença

GPL-3.0, a mesma do UxPlay que vai embutido. Componentes de terceiros dentro do `.exe`:
[UxPlay](https://github.com/FDH2/UxPlay) (GPL-3.0),
[GStreamer](https://gstreamer.freedesktop.org/) (LGPL-2.1+),
[FFmpeg](https://ffmpeg.org/) via gst-libav (LGPL/GPL) e as bibliotecas do
[MSYS2](https://www.msys2.org/) das quais eles dependem.
