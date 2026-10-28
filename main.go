// iPhone Screenshare Windows Server: icone na bandeja do Windows que mantem o UxPlay (receptor AirPlay) rodando
// para o OBS capturar a janela. O UxPlay e o GStreamer vao embutidos neste .exe (bundle/uxplay.zip,
// gerado pelo CI) e sao extraidos na primeira execucao. Escolhe FHD/HD, reinicia o UxPlay se ele
// cair e para de tentar se ele cair em loop (flag errada, porta ocupada), mostrando o log.
// Com 2+ slots, cada iPhone fica preso ao seu slot (janela "Slot N" no OBS) e volta para ele.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

const regraFirewall = "iPhone Screenshare Windows Server"

//go:embed bundle/uxplay.zip
var bundleZip []byte

type Vinculo struct {
	DeviceID string `json:"deviceID"` // deviceID que o iPhone manda no AirPlay (o UxPlay loga)
	Iphone   string `json:"iphone"`   // nome do iPhone, so para mostrar
}

type Config struct {
	Nome       string             `json:"nome"`       // nome no "Espelhar Tela"; com 2+ slots vira "LIVE 1", "LIVE 2"...
	Slots      int                `json:"slots"`      // quantos iPhones ao mesmo tempo (1 a 4)
	Qualidade  string             `json:"qualidade"`  // "FHD" ou "HD"
	UxPlay     string             `json:"uxplay"`     // caminho de outro uxplay.exe; vazio = o embutido
	ArgsExtras []string           `json:"argsExtras"` // ex.: ["-reset", "30"]
	Vinculos   map[string]Vinculo `json:"vinculos"`   // "1" -> iPhone preso ao slot 1
}

var qualidades = map[string][]string{
	"FHD": {"-s", "1920x1080", "-fps", "60"},
	"HD":  {"-s", "1280x720", "-fps", "60"},
}

var (
	cor = map[string]color.RGBA{
		"cinza": {120, 120, 120, 255}, "verde": {40, 190, 90, 255},
		"amarelo": {230, 170, 30, 255}, "vermelho": {220, 50, 50, 255},
	}
	reConexao = regexp.MustCompile(`connection request from (.*) \((.*)\) with deviceID = (\S+)`)
)

var (
	dirApp  = filepath.Join(os.Getenv("LOCALAPPDATA"), "iPhoneScreenshareServer")
	arqCfg  = filepath.Join(dirApp, "config.json")
	arqLog  = filepath.Join(dirApp, "espelho.log")
	cfg     = Config{Nome: "LIVE", Slots: 2, Qualidade: "FHD"}
	mu      sync.Mutex // protege cfg, parado e os campos dos slots
	parado  bool       // true quando o usuario pediu para sair
	slots   []*Slot
	job     windows.Handle
	logFile *os.File
)

// Slot e um UxPlay proprio (nome, portas e janela "Slot N" no OBS). O primeiro iPhone que conecta
// num slot livre fica preso a ele: o slot passa a aceitar so esse iPhone (-restrict -allow) e os
// outros slots o recusam (-block). Assim o iPhone X sempre volta para o slot X.
type Slot struct {
	N         int
	cmd       *exec.Cmd
	inicio    time.Time
	reinic    chan struct{}
	estado    string
	cor       string
	pendente  bool      // vinculo mudou; reiniciar quando o slot estiver sem ninguem espelhando
	semJanela time.Time // desde quando o UxPlay deste slot esta sem janela (ninguem espelhando)
	item      *systray.MenuItem
	desv      *systray.MenuItem
}

func (s *Slot) chave() string { return strconv.Itoa(s.N) }

func (s *Slot) nome() string {
	if cfg.Slots == 1 {
		return cfg.Nome
	}
	return fmt.Sprintf("%s %d", cfg.Nome, s.N)
}

func (s *Slot) titulo() string { return fmt.Sprintf("Slot %d", s.N) }

func main() {
	// Duas copias abertas brigariam pelos mesmos nomes e portas e cairiam em loop.
	if _, err := windows.CreateMutex(nil, false, ptr(`Local\iPhoneScreenshareServer`)); err == windows.ERROR_ALREADY_EXISTS {
		aviso("O iPhone Screenshare Windows Server ja esta aberto (icone na bandeja, perto do relogio).")
		return
	}
	os.MkdirAll(dirApp, 0o755)
	if st, err := os.Stat(arqLog); err == nil && st.Size() > 5<<20 {
		os.Rename(arqLog, arqLog+".old")
	}
	logFile, _ = os.OpenFile(arqLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	carregarConfig()
	systray.Run(aoAbrir, aoSair)
}

func logf(f string, a ...any) {
	fmt.Fprintf(logFile, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(f, a...))
}

func carregarConfig() {
	if b, err := os.ReadFile(arqCfg); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if _, ok := qualidades[cfg.Qualidade]; !ok {
		cfg.Qualidade = "FHD"
	}
	cfg.Slots = min(max(cfg.Slots, 1), 4)
	if cfg.Vinculos == nil {
		cfg.Vinculos = map[string]Vinculo{}
	}
	salvarConfig()
}

// recarregarConfig rele o config.json editado a mao. O numero de slots so muda reabrindo o app
// (o menu e montado na abertura).
func recarregarConfig() {
	mu.Lock()
	n := cfg.Slots
	cfg.Vinculos = nil
	carregarConfig()
	cfg.Slots = n
	salvarConfig()
	mu.Unlock()
	logf("config relida")
	for _, s := range slots {
		atualizarMenu(s)
	}
}

// salvarConfig grava a config; chamar com mu travado (ou antes das goroutines existirem).
func salvarConfig() {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(arqCfg, b, 0o644)
}

func aoAbrir() {
	systray.SetIcon(icone(cor["cinza"]))
	systray.SetTooltip("iPhone Screenshare")

	mStatus := systray.AddMenuItem("Preparando (1a vez demora uns segundos)...", "")
	mStatus.Disable()
	systray.AddSeparator()
	for n := 1; n <= cfg.Slots; n++ {
		s := &Slot{N: n, reinic: make(chan struct{}, 1), estado: "iniciando", cor: "cinza"}
		s.item = systray.AddMenuItem("", "")
		s.desv = s.item.AddSubMenuItem("Desvincular iPhone deste slot", "o proximo iPhone que conectar fica com ele")
		slots = append(slots, s)
		atualizarMenu(s)
	}
	systray.AddSeparator()
	mFHD := systray.AddMenuItemCheckbox("FHD 1080p", "1920x1080 a 60 fps", cfg.Qualidade == "FHD")
	mHD := systray.AddMenuItemCheckbox("HD 720p", "1280x720 a 60 fps", cfg.Qualidade == "HD")
	systray.AddSeparator()
	mReiniciar := systray.AddMenuItem("Reiniciar espelho", "")
	mLog := systray.AddMenuItem("Abrir log", "")
	mCfg := systray.AddMenuItem("Abrir config", "nome, numero de slots, argumentos extras")
	systray.AddSeparator()
	mSair := systray.AddMenuItem("Sair", "")

	exe, err := prepararAmbiente()
	if err != nil {
		logf("ERRO: %v", err)
		mStatus.SetTitle("Erro: veja o log")
		systray.SetIcon(icone(cor["vermelho"]))
		aviso(err.Error())
	} else {
		mStatus.SetTitle(fmt.Sprintf("Qualidade %s, %d slot(s)", cfg.Qualidade, cfg.Slots))
		for _, s := range slots {
			go supervisionar(s, exe)
			go func(s *Slot) {
				for range s.desv.ClickedCh {
					desvincular(s)
				}
			}(s)
		}
		go vigiarRede()
		go vigiarJanelas()
		if redePublica() {
			logf("aviso: rede do Windows marcada como Publica")
			aviso("A rede do Windows esta como PUBLICA: o firewall pode bloquear o iPhone.\n" +
				"Configuracoes > Rede e Internet > (sua rede) > Propriedades > Rede privada.")
		}
	}

	go func() {
		for {
			select {
			case <-mFHD.ClickedCh:
				trocarQualidade("FHD", mFHD, mHD, mStatus)
			case <-mHD.ClickedCh:
				trocarQualidade("HD", mFHD, mHD, mStatus)
			case <-mReiniciar.ClickedCh:
				recarregarConfig()
				reiniciarTodos()
			case <-mLog.ClickedCh:
				exec.Command("notepad.exe", arqLog).Start()
			case <-mCfg.ClickedCh:
				exec.Command("notepad.exe", arqCfg).Start()
			case <-mSair.ClickedCh:
				systray.Quit()
			}
		}
	}()
}

func aoSair() {
	mu.Lock()
	parado = true
	for _, s := range slots {
		if s.cmd != nil && s.cmd.Process != nil {
			s.cmd.Process.Kill()
		}
	}
	mu.Unlock()
	logf("app encerrado")
}

// atualizarMenu reescreve a linha do slot e a cor do icone (a pior entre os slots).
func atualizarMenu(s *Slot) {
	mu.Lock()
	defer mu.Unlock()
	quem := "livre (o 1o iPhone que conectar fica com ele)"
	if v, ok := cfg.Vinculos[s.chave()]; ok {
		quem = v.Iphone
		s.desv.Enable()
	} else {
		s.desv.Disable()
	}
	s.item.SetTitle(fmt.Sprintf("%s \"%s\": %s, %s", s.titulo(), s.nome(), quem, s.estado))

	pior := "verde"
	for _, o := range slots {
		switch {
		case o.cor == "vermelho":
			pior = "vermelho"
		case o.cor == "amarelo" && pior != "vermelho":
			pior = "amarelo"
		case o.cor == "cinza" && pior == "verde":
			pior = "cinza"
		}
	}
	systray.SetIcon(icone(cor[pior]))
	systray.SetTooltip("iPhone Screenshare: " + pior)
}

func setEstado(s *Slot, estado, c string) {
	mu.Lock()
	s.estado, s.cor = estado, c
	mu.Unlock()
	atualizarMenu(s)
}

func trocarQualidade(q string, mFHD, mHD, mStatus *systray.MenuItem) {
	if q == "FHD" {
		mFHD.Check()
		mHD.Uncheck()
	} else {
		mHD.Check()
		mFHD.Uncheck()
	}
	mu.Lock()
	igual := cfg.Qualidade == q
	cfg.Qualidade = q
	salvarConfig()
	mu.Unlock()
	if igual {
		return
	}
	logf("qualidade -> %s", q)
	mStatus.SetTitle(fmt.Sprintf("Qualidade %s, %d slot(s)", q, cfg.Slots))
	reiniciarTodos()
}

func desvincular(s *Slot) {
	mu.Lock()
	v := cfg.Vinculos[s.chave()]
	delete(cfg.Vinculos, s.chave())
	salvarConfig()
	for _, o := range slots {
		o.pendente = true
	}
	mu.Unlock()
	logf("slot %d: desvinculado de %s (%s)", s.N, v.Iphone, v.DeviceID)
	atualizarMenu(s)
}

// pedirReinicio mata o UxPlay do slot; o supervisor sobe outro com a config atual.
func pedirReinicio(s *Slot) {
	select {
	case s.reinic <- struct{}{}:
	default:
	}
	mu.Lock()
	if s.cmd != nil && s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	mu.Unlock()
}

func reiniciarTodos() {
	for _, s := range slots {
		pedirReinicio(s)
	}
}

// argsSlot monta a linha do UxPlay do slot com a config atual; chamar com mu travado.
func argsSlot(s *Slot) []string {
	// -nohold: se o Wi-Fi piscar, o iPhone reconecta na hora em vez de ser recusado pela
	// sessao antiga, que o UxPlay so derruba depois de 15 s sem resposta.
	// -p: cada slot com suas portas (n, n+1, n+2).
	args := []string{"-n", s.nome(), "-nh", "-nohold", "-p", strconv.Itoa(35000 + 10*s.N)}
	args = append(args, qualidades[cfg.Qualidade]...)
	for k, v := range cfg.Vinculos {
		switch {
		case k == s.chave():
			args = append(args, "-restrict", "-allow", v.DeviceID)
		case cfg.Slots > 1:
			args = append(args, "-block", v.DeviceID)
		}
	}
	return append(args, cfg.ArgsExtras...)
}

// lerSaida copia a saida do UxPlay para o log e aprende o vinculo iPhone -> slot.
func lerSaida(s *Slot, r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		linha := sc.Text()
		fmt.Fprintf(logFile, "[slot %d] %s\n", s.N, linha)
		m := reConexao.FindStringSubmatch(linha)
		if m == nil {
			continue
		}
		iphone, id := m[1], m[3]
		mu.Lock()
		dono, vinculado := "", false
		for k, v := range cfg.Vinculos {
			if v.DeviceID == id {
				dono, vinculado = k, true
			}
		}
		_, slotOcupado := cfg.Vinculos[s.chave()]
		novo := !vinculado && !slotOcupado && cfg.Slots > 1
		if novo {
			cfg.Vinculos[s.chave()] = Vinculo{DeviceID: id, Iphone: iphone}
			salvarConfig()
		}
		if novo || (vinculado && dono != s.chave()) {
			for _, o := range slots {
				o.pendente = true // travas valem quando cada slot ficar sem ninguem espelhando
			}
		}
		mu.Unlock()
		switch {
		case novo:
			logf("slot %d: vinculado a %s (deviceID %s)", s.N, iphone, id)
		case vinculado && dono != s.chave():
			logf("slot %d: %s pertence ao slot %s; sera recusado aqui quando este slot ficar livre", s.N, iphone, dono)
		}
		setEstado(s, "conectado: "+iphone, "verde")
	}
}

func supervisionar(s *Slot, exe string) {
	quedasRapidas := 0
	for {
		select { // pedido de reinicio feito enquanto nada rodava ja esta atendido por este inicio
		case <-s.reinic:
		default:
		}
		mu.Lock()
		args := argsSlot(s)
		s.pendente = false
		mu.Unlock()

		cmd := exec.Command(exe, args...)
		cmd.Dir = filepath.Dir(exe) // DLLs do GStreamer ficam ao lado do uxplay.exe
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW

		logf("slot %d: iniciando %s %s", s.N, exe, strings.Join(args, " "))
		if err := cmd.Start(); err != nil {
			logf("slot %d: ERRO ao iniciar: %v", s.N, err)
			setEstado(s, "erro ao iniciar, veja o log", "vermelho")
			return
		}
		go lerSaida(s, pr)
		prenderAoJob(cmd)
		mu.Lock()
		s.cmd, s.inicio, s.semJanela = cmd, time.Now(), time.Time{}
		mu.Unlock()
		setEstado(s, "esperando o iPhone", "verde")

		err := cmd.Wait()
		pw.Close()
		mu.Lock()
		sair, inicio := parado, s.inicio
		mu.Unlock()
		if sair {
			return
		}

		pedido := false
		select {
		case <-s.reinic:
			pedido = true
		default:
		}
		if pedido {
			quedasRapidas = 0
			continue
		}

		logf("slot %d: UxPlay caiu (%v) depois de %s", s.N, err, time.Since(inicio).Round(time.Second))
		if time.Since(inicio) < 10*time.Second {
			quedasRapidas++
		} else {
			quedasRapidas = 0
		}
		if quedasRapidas >= 3 {
			setEstado(s, "parado: caiu 3x seguidas, veja o log", "vermelho")
			aviso(fmt.Sprintf("O UxPlay do slot %d caiu 3 vezes seguidas logo ao abrir.\n"+
				"Veja o log (menu > Abrir log) e depois use \"Reiniciar espelho\".", s.N))
			<-s.reinic
			quedasRapidas = 0
			continue
		}
		setEstado(s, "reconectando", "amarelo")
		time.Sleep(2 * time.Second)
	}
}

var (
	user32        = windows.NewLazySystemDLL("user32.dll")
	setWindowText = user32.NewProc("SetWindowTextW")
	getWindowText = user32.NewProc("GetWindowTextW")
	janelasPorPid map[uint32][]windows.HWND
	cbJanela      = windows.NewCallback(func(h windows.HWND, _ uintptr) uintptr {
		var pid uint32
		windows.GetWindowThreadProcessId(h, &pid)
		if windows.IsWindowVisible(h) {
			janelasPorPid[pid] = append(janelasPorPid[pid], h)
		}
		return 1
	})
)

// vigiarJanelas da a cada janela de video o titulo fixo "Slot N" (o OBS acha a fonte pelo titulo)
// e aplica as travas de vinculo pendentes quando o slot esta sem ninguem espelhando.
func vigiarJanelas() {
	for range time.Tick(time.Second) {
		janelasPorPid = map[uint32][]windows.HWND{}
		windows.EnumWindows(cbJanela, nil)
		for _, s := range slots {
			mu.Lock()
			cmd, inicio, pendente := s.cmd, s.inicio, s.pendente
			mu.Unlock()
			if cmd == nil || cmd.Process == nil {
				continue
			}
			janelas := janelasPorPid[uint32(cmd.Process.Pid)]
			for _, h := range janelas {
				if tituloJanela(h) != s.titulo() {
					setWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(ptr(s.titulo()))))
				}
			}
			mu.Lock()
			if len(janelas) > 0 {
				s.semJanela = time.Time{}
			} else if s.semJanela.IsZero() {
				s.semJanela = time.Now()
			}
			ocioso := !s.semJanela.IsZero() && time.Since(s.semJanela) > 5*time.Second
			if len(janelas) == 0 && strings.HasPrefix(s.estado, "conectado") {
				s.estado = "esperando o iPhone"
			}
			mu.Unlock()
			if pendente && ocioso && time.Since(inicio) > 5*time.Second {
				logf("slot %d: aplicando vinculos (slot sem ninguem espelhando)", s.N)
				pedirReinicio(s)
			}
			atualizarMenu(s)
		}
	}
}

func tituloJanela(h windows.HWND) string {
	buf := make([]uint16, 256)
	getWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

// prepararAmbiente extrai o UxPlay embutido (uma vez por versao) e cria a regra de firewall uma vez.
func prepararAmbiente() (string, error) {
	exe := cfg.UxPlay
	if exe == "" {
		dir, err := extrairBundle()
		if err != nil {
			return "", fmt.Errorf("Nao consegui extrair o UxPlay embutido: %v", err)
		}
		exe = filepath.Join(dir, "bin", "uxplay.exe")
		raiz := dir
		os.Setenv("PATH", filepath.Join(raiz, "bin")+";"+os.Getenv("PATH"))
		os.Setenv("GST_PLUGIN_SYSTEM_PATH_1_0", filepath.Join(raiz, "lib", "gstreamer-1.0"))
		os.Setenv("GST_PLUGIN_PATH_1_0", filepath.Join(raiz, "lib", "gstreamer-1.0"))
		os.Setenv("GST_PLUGIN_SCANNER_1_0", filepath.Join(raiz, "libexec", "gstreamer-1.0", "gst-plugin-scanner.exe"))
		os.Setenv("GST_REGISTRY_1_0", filepath.Join(raiz, "registry.bin"))
		os.Setenv("FONTCONFIG_FILE", filepath.Join(raiz, "etc", "fonts", "fonts.conf"))
	} else if _, err := os.Stat(exe); err != nil {
		return "", fmt.Errorf("O \"uxplay\" do config nao existe: %s\nApague a linha para usar o embutido.\nConfig: %s", exe, arqCfg)
	}
	logf("uxplay: %s", exe)

	regra, _ := oculto("netsh", "advfirewall", "firewall", "show", "rule", "name="+regraFirewall, "verbose").CombinedOutput()
	if !strings.Contains(strings.ToLower(string(regra)), strings.ToLower(exe)) {
		logf("criando/atualizando regra de firewall (pede admin)")
		params := fmt.Sprintf(`/c netsh advfirewall firewall delete rule name="%[1]s" & `+
			`netsh advfirewall firewall add rule name="%[1]s" dir=in action=allow program="%[2]s" profile=private,domain`, regraFirewall, exe)
		windows.ShellExecute(0, ptr("runas"), ptr("cmd"), ptr(params), nil, windows.SW_HIDE)
	}
	return exe, nil
}

// oculto monta um comando de console que nao abre janela preta (o app e GUI).
func oculto(nome string, args ...string) *exec.Cmd {
	cmd := exec.Command(nome, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd
}

func redePublica() bool {
	out, _ := oculto("powershell", "-NoProfile", "-Command", "(Get-NetConnectionProfile).NetworkCategory").Output()
	return strings.Contains(string(out), "Public")
}

// vigiarRede reinicia o UxPlay quando os IPs do PC mudam (cabo/Wi-Fi caiu e voltou, IP novo do
// roteador) ou quando o PC volta da suspensao: o anuncio mDNS fica preso a rede antiga e o
// iPhone para de achar o "LIVE".
func vigiarRede() {
	antes, tick := ipsLocais(), time.Now().Round(0) // Round(0): relogio de parede, conta a suspensao
	for range time.Tick(5 * time.Second) {
		agora, ips := time.Now().Round(0), ipsLocais()
		switch {
		case agora.Sub(tick) > 30*time.Second:
			logf("PC voltou da suspensao; reiniciando o UxPlay")
			reiniciarTodos()
		case ips != antes && ips != "":
			logf("rede mudou (%s -> %s); reiniciando o UxPlay", antes, ips)
			reiniciarTodos()
		}
		if ips != "" {
			antes = ips
		}
		tick = agora
	}
}

func ipsLocais() string {
	var ips []string
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				ips = append(ips, n.IP.String())
			}
		}
	}
	sort.Strings(ips)
	return strings.Join(ips, ",")
}

// extrairBundle descompacta o zip embutido numa pasta com o hash dele no nome; se a pasta ja
// estiver completa (arquivo .ok), reaproveita. Versao nova do app = pasta nova.
func extrairBundle() (string, error) {
	h := sha256.Sum256(bundleZip)
	dir := filepath.Join(dirApp, "uxplay-"+hex.EncodeToString(h[:6]))
	ok := filepath.Join(dir, ".ok")
	if _, err := os.Stat(ok); err == nil {
		return dir, nil
	}
	logf("extraindo UxPlay para %s", dir)
	os.RemoveAll(dir)
	zr, err := zip.NewReader(bytes.NewReader(bundleZip), int64(len(bundleZip)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		dest := filepath.Join(dir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(dest, dir+string(os.PathSeparator)) {
			return "", fmt.Errorf("caminho invalido no zip: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(dest, 0o755)
			continue
		}
		if err := extrairArquivo(f, dest); err != nil {
			return "", err
		}
	}
	return dir, os.WriteFile(ok, nil, 0o644)
}

func extrairArquivo(f *zip.File, dest string) error {
	os.MkdirAll(filepath.Dir(dest), 0o755)
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer w.Close()
	_, err = io.Copy(w, r)
	return err
}

// prenderAoJob faz o Windows matar o UxPlay junto se este app fechar de qualquer jeito.
func prenderAoJob(cmd *exec.Cmd) {
	if job == 0 {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		windows.SetInformationJobObject(h, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
		job = h
	}
	if p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
		windows.AssignProcessToJobObject(job, p)
		windows.CloseHandle(p)
	}
}

func aviso(msg string) {
	windows.MessageBox(0, ptr(msg), ptr("iPhone Screenshare"), windows.MB_OK|windows.MB_ICONWARNING)
}

func ptr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// icone desenha uma bolinha da cor do status e embrulha o PNG num .ico (aceito desde o Vista).
func icone(c color.RGBA) []byte {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			if dx*dx+dy*dy <= 14*14 {
				img.Set(x, y, c)
			}
		}
	}
	var p bytes.Buffer
	png.Encode(&p, img)

	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, 1})                 // cabecalho: tipo icone, 1 imagem
	ico.Write([]byte{n, n, 0, 0})                                              // largura, altura, cores, reservado
	binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})                   // planos, bits
	binary.Write(&ico, binary.LittleEndian, []uint32{uint32(p.Len()), 6 + 16}) // tamanho, offset
	ico.Write(p.Bytes())
	return ico.Bytes()
}
