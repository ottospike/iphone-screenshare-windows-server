# Espelho iPhone

Recebe o **espelhamento de tela do iPhone (AirPlay)** no Windows pelo Wi-Fi, numa janela que o
**OBS** captura. É um único `.exe`: sem instalador, sem Bonjour, sem conta, sem trial.

Por baixo roda o [UxPlay](https://github.com/FDH2/UxPlay) (receptor AirPlay open source) com o
mDNS embutido dele, então o iPhone acha o PC sem precisar do Bonjour da Apple. O app fica na bandeja
do Windows, mantém o UxPlay no ar e reinicia se ele cair.

## Download

Baixe o `EspelhoIphone.exe` da [última release](../../releases/latest).
Requisitos: Windows 10 ou 11 (64 bits), iPhone e PC **na mesma rede**.

## Uso

1. Abra o `EspelhoIphone.exe`. Na primeira vez:
   - ele extrai o UxPlay para `%LOCALAPPDATA%\EspelhoIphone\` (alguns segundos);
   - o Windows pede permissão de administrador **uma vez**, para liberar o firewall.
2. No iPhone: **Central de Controle → Espelhar Tela → LIVE**.
3. No OBS: **Fontes → + → Captura de Janela**
   - Janela: `[uxplay.exe]`
   - Método de captura: **Windows 10 (1903 e superior)**
   - Prioridade de correspondência: **Corresponder a janela do mesmo executável**. A janela do
     UxPlay fecha quando o iPhone desconecta e volta quando ele reconecta; assim o OBS a reencontra.

## Bandeja (o painel de controle)

Clique no ícone da bandeja (perto do relógio):

| Item | O que faz |
|---|---|
| Status (1ª linha) | `No ar: "LIVE" em FHD`, `Reconectando...` ou o erro |
| **FHD 1080p / HD 720p** | Qualidade. A escolha fica salva. Trocar reinicia o UxPlay: espelhe de novo no iPhone |
| Reiniciar espelho | Derruba e sobe o UxPlay de novo |
| Abrir log | Log do app e do UxPlay no Bloco de Notas |
| Abrir config | `config.json` no Bloco de Notas |
| Sair | Fecha o app e o UxPlay |

A cor do ícone resume o estado: **verde** no ar, **amarelo** reconectando, **vermelho** erro,
**cinza** iniciando.

Se o UxPlay cair, o app sobe ele de novo em 2 s. Se cair 3 vezes seguidas logo ao abrir, o app para
e avisa (para não ficar em loop); veja o log e use **Reiniciar espelho**.

## Config

`%LOCALAPPDATA%\EspelhoIphone\config.json` (menu → Abrir config). Depois de editar, use
**Reiniciar espelho**.

```json
{
  "nome": "LIVE",
  "qualidade": "FHD",
  "uxplay": "",
  "argsExtras": []
}
```

| Campo | Para quê |
|---|---|
| `nome` | Nome que aparece em **Espelhar Tela** no iPhone |
| `qualidade` | `FHD` (1920x1080) ou `HD` (1280x720), ambos a 60 fps |
| `uxplay` | Caminho de outro `uxplay.exe`. Vazio = o embutido |
| `argsExtras` | Opções a mais do UxPlay, ex.: `["-nohold"]` (o iPhone novo derruba o atual) ou `["-pin", "1234"]` |

Um iPhone por vez: com alguém espelhando, outro iPhone que tentar é recusado (padrão do UxPlay).

## Dicas para a live

- PC no **cabo de rede**; iPhone no Wi-Fi de **5 GHz** do mesmo roteador.
- Se outro iPhone for a câmera (Iriun etc.), ligue esse pelo **USB** para não dividir o Wi-Fi.
- No iPhone espelhado: **Não Perturbe** ligado e bloqueio automático desligado.
- O AirPlay tem uns 100–200 ms de atraso; se o áudio vier dele, ajuste a sincronia no OBS
  (Propriedades avançadas de áudio).

## Problemas comuns

| Sintoma | O que fazer |
|---|---|
| "LIVE" não aparece no iPhone | Mesma rede? Rede de convidados e "isolamento de clientes" do roteador bloqueiam. Rede do Windows como **Privada** (a regra de firewall vale para Privada/Domínio) |
| Aparece mas não conecta | Firewall: confira a regra "Espelho iPhone (UxPlay)" em *Firewall do Windows → Regras de entrada*. Reabra o app |
| Tela preta no OBS | Troque o método de captura da Captura de Janela, ou use Captura de Tela com corte |
| Ícone vermelho | Menu → Abrir log; a última linha diz o motivo |
| Antivírus reclama | Libere a pasta `%LOCALAPPDATA%\EspelhoIphone\` |

## Como funciona

```
EspelhoIphone.exe (Go, bandeja)
 ├─ bundle/uxplay.zip embutido ── extraído 1x ──> %LOCALAPPDATA%\EspelhoIphone\uxplay-<hash>\
 │                                                 bin\uxplay.exe + DLLs, plugins do GStreamer
 ├─ sobe uxplay.exe -n LIVE -nh -s 1920x1080 -fps 60 (sem console, saída no log)
 ├─ vigia o processo: caiu → sobe de novo; 3 quedas rápidas → para e avisa
 └─ job object do Windows: se o app fechar de qualquer jeito, o uxplay fecha junto
```

O UxPlay e o GStreamer são compilados no GitHub Actions (Windows + MSYS2 UCRT64) a partir de um
commit fixo do [FDH2/UxPlay](https://github.com/FDH2/UxPlay) (≥ 1.74, que traz o mDNS próprio).
O CI também testa o resultado num Windows limpo: todos os elementos do GStreamer presentes, o
`uxplay.exe` sobe sozinho, e o `EspelhoIphone.exe` extrai e mantém o UxPlay no ar.

## Build

O `.exe` sai do workflow [`build`](.github/workflows/build.yml):

- todo push na `main` gera o artifact `EspelhoIphone`;
- uma tag `v*` publica a release.

Para atualizar o UxPlay, troque `UXPLAY_REF` no workflow pelo commit novo.

Local (precisa do `bundle/uxplay.zip` gerado pelo CI):

```
GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -s -w" -o dist/EspelhoIphone.exe .
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
