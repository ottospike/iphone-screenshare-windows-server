// Android: o scrcpy (com o adb que vem junto) espelha cada Android numa janela "Android N" para o OBS.
// Pelo Wi-Fi usa a Depuracao por Wi-Fi do Android 11+: o celular e pareado uma vez pelo QR code (menu)
// e depois o adb acha e conecta ele sozinho pelo mDNS sempre que a Depuracao por Wi-Fi estiver ligada.
// Pelo USB basta a Depuracao USB. Cada Android fica preso ao seu slot pelo numero de serie, que e o
// mesmo no USB e no Wi-Fi.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"fyne.io/systray"
	"github.com/skip2/go-qrcode"
)

type VinculoAndroid struct {
	Serial   string `json:"serial"`   // ro.serialno do aparelho
	Aparelho string `json:"aparelho"` // modelo, so para mostrar
}

// SlotAndroid e um scrcpy proprio, com a janela "Android N". O primeiro Android que aparece com o
// slot livre fica preso a ele; depois o slot so abre aquele Android.
type SlotAndroid struct {
	N       int
	cmd     *exec.Cmd
	ocupado bool      // tem scrcpy rodando (ou subindo) neste slot
	pedido  bool      // o app matou o scrcpy (qualidade, reiniciar): nao conta como queda
	espera  time.Time // caiu 3x seguidas logo ao abrir: nao abre de novo antes disso
	quedas  int
	estado  string
	cor     string
	item    *systray.MenuItem
	desv    *systray.MenuItem
}

func (s *SlotAndroid) chave() string  { return strconv.Itoa(s.N) }
func (s *SlotAndroid) titulo() string { return fmt.Sprintf("Android %d", s.N) }

// aparelho e um Android que o adb ve pronto ("device").
type aparelho struct{ adbSerial, serial, modelo string }

// wifi: o adb chama o aparelho pelo IP:porta ou pelo nome mDNS quando a conexao e pelo Wi-Fi.
func (a aparelho) wifi() bool {
	return strings.Contains(a.adbSerial, ":") || strings.Contains(a.adbSerial, "._adb-tls-")
}

func (a aparelho) via() string {
	if a.wifi() {
		return "Wi-Fi"
	}
	return "USB"
}

var tamanhos = map[string]string{"FHD": "1920", "HD": "1280"} // --max-size (lado maior)

var (
	slotsAndroid      []*SlotAndroid
	adbExe, scrcpyExe string
	pareando          atomic.Bool
	// so a goroutine vigiarAndroid usa:
	infoAdb   = map[string]aparelho{}  // adbSerial -> aparelho
	tentado   = map[string]time.Time{} // IP:porta -> ultimo "adb connect"
	avisadoNA = map[string]bool{}      // adbSerial ja logado como "sem autorizar"
)

func prepararAndroid() error {
	dir, err := extrairBundle("scrcpy", bundleScrcpy)
	if err != nil {
		return fmt.Errorf("Nao consegui extrair o scrcpy embutido: %v", err)
	}
	scrcpyExe, adbExe = filepath.Join(dir, "scrcpy.exe"), filepath.Join(dir, "adb.exe")
	logf("scrcpy: %s", scrcpyExe)
	// Sobe o servidor do adb sem pipes: se ele herdasse os pipes de um comando nosso, o Wait desse
	// comando esperaria o servidor fechar.
	oculto(adbExe, "start-server").Run()
	logf("adb mdns: %s", strings.TrimSpace(adb("mdns", "check")))
	return nil
}

// adb roda um comando do adb e devolve a saida; nunca trava mais que 10 s.
func adb(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, adbExe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	cmd.WaitDelay = time.Second
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// vigiarAndroid conecta os Androids pareados que aparecem no Wi-Fi e abre um scrcpy por slot.
func vigiarAndroid() {
	for range time.Tick(2 * time.Second) {
		mu.Lock()
		sair := parado
		mu.Unlock()
		if sair {
			return
		}
		devs := adb("devices")
		conectarWifi(devs)
		distribuir(prontos(devs))
	}
}

// conectarWifi faz "adb connect" nos Androids que anunciam a Depuracao por Wi-Fi e ainda nao estao no
// adb. So os ja pareados com este PC conectam; os outros falham e so sao tentados de novo em 30 s.
func conectarWifi(devs string) {
	for _, l := range strings.Split(adb("mdns", "services"), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || !strings.HasPrefix(f[1], "_adb-tls-connect") {
			continue
		}
		nome, end := f[0], f[2]
		if strings.Contains(devs, nome) || strings.Contains(devs, end) || time.Since(tentado[end]) < 30*time.Second {
			continue
		}
		tentado[end] = time.Now()
		logf("android: adb connect %s (%s): %s", end, nome, strings.TrimSpace(adb("connect", end)))
	}
}

// prontos le a saida do "adb devices" e devolve os Androids prontos, USB primeiro (mais estavel).
func prontos(devs string) []aparelho {
	var lista []aparelho
	for _, l := range strings.Split(devs, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch f[1] {
		case "unauthorized":
			if !avisadoNA[f[0]] {
				avisadoNA[f[0]] = true
				logf("android: %s esperando o \"Permitir depuracao\" no celular", f[0])
			}
		case "device":
			a, ok := infoAdb[f[0]]
			if !ok {
				p := strings.Split(strings.TrimSpace(adb("-s", f[0], "shell", "getprop ro.serialno; getprop ro.product.model")), "\n")
				if len(p) < 2 || strings.TrimSpace(p[0]) == "" {
					continue
				}
				a = aparelho{f[0], strings.TrimSpace(p[0]), strings.TrimSpace(p[1])}
				infoAdb[f[0]] = a
				logf("android: %s visto (%s, serie %s)", a.modelo, a.adbSerial, a.serial)
			}
			lista = append(lista, a)
		}
	}
	sort.SliceStable(lista, func(i, j int) bool { return !lista[i].wifi() && lista[j].wifi() })
	return lista
}

// distribuir abre um scrcpy em cada slot livre: o Android vinculado a ele ou, se o slot nao tem
// vinculo, o primeiro Android sem slot (que passa a ser dele).
func distribuir(lista []aparelho) {
	type abrir struct {
		s *SlotAndroid
		a aparelho
	}
	var fila []abrir
	mu.Lock()
	if parado {
		mu.Unlock()
		return
	}
	emUso := map[string]bool{}
	for _, s := range slotsAndroid {
		if v, ok := cfg.VinculosAndroid[s.chave()]; ok && s.ocupado {
			emUso[v.Serial] = true
		}
	}
	for _, s := range slotsAndroid {
		if s.ocupado || time.Now().Before(s.espera) {
			continue
		}
		v, preso := cfg.VinculosAndroid[s.chave()]
		for _, a := range lista {
			if emUso[a.serial] || (preso && a.serial != v.Serial) || (!preso && donoAndroid(a.serial) != "") {
				continue
			}
			if !preso && cfg.Android > 1 {
				cfg.VinculosAndroid[s.chave()] = VinculoAndroid{Serial: a.serial, Aparelho: a.modelo}
				salvarConfig()
				logf("android %d: vinculado a %s (serie %s)", s.N, a.modelo, a.serial)
			}
			emUso[a.serial], s.ocupado = true, true
			fila = append(fila, abrir{s, a})
			break
		}
	}
	mu.Unlock()
	for _, x := range fila {
		go rodarScrcpy(x.s, x.a)
	}
}

// donoAndroid diz a que slot o Android esta vinculado ("" = nenhum); chamar com mu travado.
func donoAndroid(serial string) string {
	for k, v := range cfg.VinculosAndroid {
		if v.Serial == serial {
			return k
		}
	}
	return ""
}

func rodarScrcpy(s *SlotAndroid, a aparelho) {
	mu.Lock()
	args := []string{"-s", a.adbSerial, "--window-title", s.titulo(), "--max-size", tamanhos[cfg.Qualidade], "--max-fps", "60"}
	args = append(args, cfg.ArgsScrcpy...)
	mu.Unlock()

	cmd := exec.Command(scrcpyExe, args...)
	cmd.Dir = filepath.Dir(scrcpyExe)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW

	logf("android %d: iniciando %s %s", s.N, scrcpyExe, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		logf("android %d: ERRO ao iniciar: %v", s.N, err)
		mu.Lock()
		s.ocupado, s.espera = false, time.Now().Add(30*time.Second)
		mu.Unlock()
		setEstadoAndroid(s, "erro ao iniciar, veja o log", "vermelho")
		return
	}
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			fmt.Fprintf(logFile, "[android %d] %s\n", s.N, sc.Text())
		}
	}()
	prenderAoJob(cmd)
	inicio := time.Now()
	mu.Lock()
	s.cmd = cmd
	mu.Unlock()
	setEstadoAndroid(s, fmt.Sprintf("conectado: %s (%s)", a.modelo, a.via()), "verde")

	err := cmd.Wait()
	pw.Close()
	mu.Lock()
	s.cmd, s.ocupado = nil, false
	sair, pedido := parado, s.pedido
	s.pedido = false
	switch {
	case pedido || time.Since(inicio) >= 10*time.Second:
		s.quedas = 0
	default:
		s.quedas++
	}
	travou := s.quedas >= 3
	if travou {
		s.quedas, s.espera = 0, time.Now().Add(30*time.Second)
	}
	mu.Unlock()
	if sair {
		return
	}
	if !pedido {
		logf("android %d: scrcpy saiu (%v) depois de %s", s.N, err, time.Since(inicio).Round(time.Second))
	}
	if travou {
		// Sem janela de aviso: no meio da live o Wi-Fi do celular pode piscar; tenta de novo sozinho.
		logf("android %d: caiu 3x seguidas logo ao abrir; tentando de novo em 30 s", s.N)
		setEstadoAndroid(s, "falhou 3x, tentando de novo em 30 s (veja o log)", "amarelo")
		return
	}
	setEstadoAndroid(s, "esperando o Android", "verde")
}

// reiniciarAndroids fecha os scrcpy abertos (o vigiarAndroid abre de novo com a config atual) e
// tira os slots da espera.
func reiniciarAndroids() {
	mu.Lock()
	for _, s := range slotsAndroid {
		s.espera, s.quedas = time.Time{}, 0
		if s.cmd != nil && s.cmd.Process != nil {
			s.pedido = true
			s.cmd.Process.Kill()
		}
	}
	mu.Unlock()
	for _, s := range slotsAndroid {
		atualizarMenuAndroid(s)
	}
}

func desvincularAndroid(s *SlotAndroid) {
	mu.Lock()
	v := cfg.VinculosAndroid[s.chave()]
	delete(cfg.VinculosAndroid, s.chave())
	salvarConfig()
	mu.Unlock()
	logf("android %d: desvinculado de %s (serie %s)", s.N, v.Aparelho, v.Serial)
	atualizarMenuAndroid(s)
}

func setEstadoAndroid(s *SlotAndroid, estado, c string) {
	mu.Lock()
	s.estado, s.cor = estado, c
	mu.Unlock()
	atualizarMenuAndroid(s)
}

func atualizarMenuAndroid(s *SlotAndroid) {
	mu.Lock()
	defer mu.Unlock()
	quem := "livre (o 1o Android que conectar fica com ele)"
	if v, ok := cfg.VinculosAndroid[s.chave()]; ok {
		quem = v.Aparelho
		s.desv.Enable()
	} else {
		s.desv.Disable()
	}
	s.item.SetTitle(fmt.Sprintf("%s: %s, %s", s.titulo(), quem, s.estado))
	atualizarIcone()
}

// parearQR mostra um QR code de pareamento (o mesmo formato do Android Studio). O celular le o QR em
// Depuracao por Wi-Fi > Parear dispositivo com QR code e anuncia o pareamento no mDNS com o nome do
// QR; o app acha esse anuncio e roda "adb pair" com a senha do QR.
func parearQR() {
	if !pareando.CompareAndSwap(false, true) {
		aviso("Ja tem um pareamento aberto. Leia o QR code que esta no navegador.")
		return
	}
	defer pareando.Store(false)
	nome, senha := "studio-"+aleatorio(3), aleatorio(4)
	png, err := qrcode.Encode("WIFI:T:ADB;S:"+nome+";P:"+senha+";;", qrcode.Medium, 360)
	if err != nil {
		aviso("Nao consegui gerar o QR code: " + err.Error())
		return
	}
	arq := filepath.Join(dirApp, "parear-android.html")
	os.WriteFile(arq, []byte(fmt.Sprintf(paginaParear, base64.StdEncoding.EncodeToString(png))), 0o644)
	exec.Command("rundll32", "url.dll,FileProtocolHandler", arq).Start()
	logf("android: QR de pareamento aberto (%s)", nome)

	for fim := time.Now().Add(3 * time.Minute); time.Now().Before(fim); time.Sleep(time.Second) {
		for _, l := range strings.Split(adb("mdns", "services"), "\n") {
			f := strings.Fields(l)
			if len(f) < 3 || f[0] != nome || !strings.HasPrefix(f[1], "_adb-tls-pairing") {
				continue
			}
			out := strings.TrimSpace(adb("pair", f[2], senha))
			logf("android: adb pair %s: %s", f[2], out)
			if strings.Contains(out, "Successfully paired") {
				aviso("Android pareado! Ele conecta sozinho em alguns segundos e aparece num slot Android.\n" +
					"Pode fechar a pagina do QR code.")
			} else {
				aviso("O pareamento falhou:\n" + out + "\n\nTente de novo pelo menu.")
			}
			return
		}
	}
	logf("android: QR de pareamento expirou")
	aviso("Nenhum Android leu o QR code em 3 minutos.\n" +
		"Confira se o celular e o PC estao na mesma rede Wi-Fi e tente de novo pelo menu.")
}

func aleatorio(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const paginaParear = `<!doctype html>
<html lang="pt-BR"><head><meta charset="utf-8"><title>Parear Android</title>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;background:#111;color:#eee;font:16px/1.5 system-ui,sans-serif}
main{max-width:420px;padding:24px;text-align:center}
img{background:#fff;padding:16px;border-radius:12px;width:320px;height:320px}
ol{text-align:left;padding-left:20px}
</style></head><body><main>
<h1>Parear Android pelo Wi-Fi</h1>
<img src="data:image/png;base64,%s" alt="QR code de pareamento">
<ol>
<li>Celular e PC na <b>mesma rede</b>.</li>
<li>No celular: <b>Configuracoes &gt; Opcoes do desenvolvedor &gt; Depuracao por Wi-Fi</b>: ligue.</li>
<li>Toque em <b>Parear dispositivo com QR code</b> e aponte para este QR.</li>
<li>O app avisa quando parear. Depois e so deixar a Depuracao por Wi-Fi ligada.</li>
</ol>
<p>Este QR vale por 3 minutos.</p>
</main></body></html>`
