// Espelho iPhone: icone na bandeja do Windows que mantem o UxPlay (receptor AirPlay) rodando
// para o OBS capturar a janela. O UxPlay e o GStreamer vao embutidos neste .exe (bundle/uxplay.zip,
// gerado pelo CI) e sao extraidos na primeira execucao. Escolhe FHD/HD, reinicia o UxPlay se ele
// cair e para de tentar se ele cair em loop (flag errada, porta ocupada), mostrando o log.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/systray"
	"golang.org/x/sys/windows"
)

const regraFirewall = "Espelho iPhone (UxPlay)"

//go:embed bundle/uxplay.zip
var bundleZip []byte

type Config struct {
	Nome       string   `json:"nome"`       // nome que aparece no "Espelhar Tela" do iPhone
	Qualidade  string   `json:"qualidade"`  // "FHD" ou "HD"
	UxPlay     string   `json:"uxplay"`     // caminho de outro uxplay.exe; vazio = o embutido
	ArgsExtras []string `json:"argsExtras"` // ex.: ["-pin", "1234"]
}

var qualidades = map[string][]string{
	"FHD": {"-s", "1920x1080", "-fps", "60"},
	"HD":  {"-s", "1280x720", "-fps", "60"},
}

var (
	dirApp  = filepath.Join(os.Getenv("LOCALAPPDATA"), "EspelhoIphone")
	arqCfg  = filepath.Join(dirApp, "config.json")
	arqLog  = filepath.Join(dirApp, "espelho.log")
	cfg     = Config{Nome: "LIVE", Qualidade: "FHD"}
	mu      sync.Mutex
	atual   *exec.Cmd
	parado  bool // true quando o usuario pediu para sair
	reinic  = make(chan struct{}, 1)
	job     windows.Handle
	logFile *os.File
)

func main() {
	os.MkdirAll(dirApp, 0o755)
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
	salvarConfig()
}

func salvarConfig() {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(arqCfg, b, 0o644)
}

func aoAbrir() {
	systray.SetIcon(icone(color.RGBA{120, 120, 120, 255}))
	systray.SetTooltip("Espelho iPhone")

	mStatus := systray.AddMenuItem("Iniciando...", "")
	mStatus.Disable()
	systray.AddSeparator()
	mFHD := systray.AddMenuItemCheckbox("FHD 1080p", "1920x1080 a 60 fps", cfg.Qualidade == "FHD")
	mHD := systray.AddMenuItemCheckbox("HD 720p", "1280x720 a 60 fps", cfg.Qualidade == "HD")
	systray.AddSeparator()
	mReiniciar := systray.AddMenuItem("Reiniciar espelho", "")
	mLog := systray.AddMenuItem("Abrir log", "")
	mCfg := systray.AddMenuItem("Abrir config", "nome no iPhone, argumentos extras")
	systray.AddSeparator()
	mSair := systray.AddMenuItem("Sair", "")

	status := func(txt string, c color.RGBA) {
		mStatus.SetTitle(txt)
		systray.SetIcon(icone(c))
		systray.SetTooltip("Espelho iPhone: " + txt)
	}

	status("Preparando (1a vez demora uns segundos)...", color.RGBA{120, 120, 120, 255})
	exe, err := prepararAmbiente()
	if err != nil {
		logf("ERRO: %v", err)
		status("Erro: veja o log", color.RGBA{220, 50, 50, 255})
		aviso(err.Error())
	} else {
		go supervisionar(exe, status)
	}

	go func() {
		for {
			select {
			case <-mFHD.ClickedCh:
				trocarQualidade("FHD", mFHD, mHD)
			case <-mHD.ClickedCh:
				trocarQualidade("HD", mFHD, mHD)
			case <-mReiniciar.ClickedCh:
				pedirReinicio()
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
	if atual != nil && atual.Process != nil {
		atual.Process.Kill()
	}
	mu.Unlock()
	logf("app encerrado")
}

func trocarQualidade(q string, mFHD, mHD *systray.MenuItem) {
	if q == "FHD" {
		mFHD.Check()
		mHD.Uncheck()
	} else {
		mHD.Check()
		mFHD.Uncheck()
	}
	if cfg.Qualidade == q {
		return
	}
	cfg.Qualidade = q
	salvarConfig()
	logf("qualidade -> %s", q)
	pedirReinicio()
}

// pedirReinicio mata o UxPlay atual; o supervisor sobe outro com a config nova.
func pedirReinicio() {
	select {
	case reinic <- struct{}{}:
	default:
	}
	mu.Lock()
	if atual != nil && atual.Process != nil {
		atual.Process.Kill()
	}
	mu.Unlock()
}

func supervisionar(exe string, status func(string, color.RGBA)) {
	quedasRapidas := 0
	for {
		args := append([]string{"-n", cfg.Nome, "-nh"}, qualidades[cfg.Qualidade]...)
		args = append(args, cfg.ArgsExtras...)
		cmd := exec.Command(exe, args...)
		cmd.Dir = filepath.Dir(exe) // DLLs do GStreamer ficam ao lado do uxplay.exe
		cmd.Stdout, cmd.Stderr = logFile, logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW

		logf("iniciando: %s %s", exe, strings.Join(args, " "))
		inicio := time.Now()
		if err := cmd.Start(); err != nil {
			logf("ERRO ao iniciar: %v", err)
			status("Erro ao iniciar: veja o log", color.RGBA{220, 50, 50, 255})
			return
		}
		prenderAoJob(cmd)
		mu.Lock()
		atual = cmd
		mu.Unlock()
		status(fmt.Sprintf("No ar: \"%s\" em %s", cfg.Nome, cfg.Qualidade), color.RGBA{40, 190, 90, 255})

		err := cmd.Wait()
		mu.Lock()
		sair := parado
		mu.Unlock()
		if sair {
			return
		}

		pedido := false
		select {
		case <-reinic:
			pedido = true
		default:
		}
		if pedido {
			quedasRapidas = 0
			continue
		}

		logf("UxPlay caiu (%v) depois de %s", err, time.Since(inicio).Round(time.Second))
		if time.Since(inicio) < 10*time.Second {
			quedasRapidas++
		} else {
			quedasRapidas = 0
		}
		if quedasRapidas >= 3 {
			status("Parado: caiu 3x seguidas, veja o log", color.RGBA{220, 50, 50, 255})
			aviso("O UxPlay caiu 3 vezes seguidas logo ao abrir.\nVeja o log (menu > Abrir log) e depois use \"Reiniciar espelho\".")
			<-reinic
			quedasRapidas = 0
			continue
		}
		status("Reconectando...", color.RGBA{230, 170, 30, 255})
		time.Sleep(2 * time.Second)
	}
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

	if exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+regraFirewall).Run() != nil {
		logf("criando regra de firewall (pede admin)")
		params := fmt.Sprintf(`advfirewall firewall add rule name="%s" dir=in action=allow program="%s" profile=private,domain`, regraFirewall, exe)
		windows.ShellExecute(0, ptr("runas"), ptr("netsh"), ptr(params), nil, windows.SW_HIDE)
	}
	return exe, nil
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
	windows.MessageBox(0, ptr(msg), ptr("Espelho iPhone"), windows.MB_OK|windows.MB_ICONWARNING)
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
	binary.Write(&ico, binary.LittleEndian, []uint16{0, 1, 1})                // cabecalho: tipo icone, 1 imagem
	ico.Write([]byte{n, n, 0, 0})                                             // largura, altura, cores, reservado
	binary.Write(&ico, binary.LittleEndian, []uint16{1, 32})                  // planos, bits
	binary.Write(&ico, binary.LittleEndian, []uint32{uint32(p.Len()), 6 + 16}) // tamanho, offset
	ico.Write(p.Bytes())
	return ico.Bytes()
}
