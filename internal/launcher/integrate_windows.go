package launcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/korvin2000/java-runner/internal/ui"
)

var iconExts = []string{".ico"}

const uninstallKey = `HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\`

// createIntegrations creates .lnk shortcuts, the entry in Settings > Apps
// (with an uninstall command) and the user PATH entry, using one PowerShell
// invocation (available on every supported Windows version).
func createIntegrations(in integration) ([]string, error) {
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	if in.Desktop || in.Menu {
		fmt.Fprintf(&b, "$ws = New-Object -ComObject WScript.Shell\n")
		fmt.Fprintf(&b, "function New-Link($dir) { $p = Join-Path $dir %s; $s = $ws.CreateShortcut($p); "+
			"$s.TargetPath = %s; $s.WorkingDirectory = %s; $s.Description = %s; if (%s) { $s.IconLocation = %s }; "+
			"$s.Save(); Write-Output $p }\n",
			psq(in.fileName()+".lnk"), psq(in.Launcher), psq(in.InstallDir), psq(in.Name), psq(in.Icon), psq(in.Icon))
		if in.Desktop {
			b.WriteString("New-Link ([Environment]::GetFolderPath('Desktop'))\n")
		}
		if in.Menu {
			b.WriteString("New-Link ([Environment]::GetFolderPath('Programs'))\n")
		}
	}
	icon := in.Icon
	if icon == "" {
		icon = in.Launcher
	}
	fmt.Fprintf(&b, "$k = %s\nNew-Item -Path $k -Force | Out-Null\n", psq(uninstallKey+in.ID))
	for _, kv := range [][2]string{
		{"DisplayName", in.Name}, {"DisplayVersion", in.Version}, {"Publisher", in.Publisher},
		{"InstallLocation", in.InstallDir}, {"DisplayIcon", icon},
		{"UninstallString", `"` + in.Launcher + `" ` + flagPrefix + "uninstall"},
	} {
		fmt.Fprintf(&b, "Set-ItemProperty -Path $k -Name %s -Value %s\n", kv[0], psq(kv[1]))
	}
	b.WriteString("Set-ItemProperty -Path $k -Name NoModify -Value 1 -Type DWord\n")
	b.WriteString("Set-ItemProperty -Path $k -Name NoRepair -Value 1 -Type DWord\n")
	if in.Path {
		b.WriteString(pathScript(in.InstallDir, true))
	}

	out, err := powershell(b.String())
	var created []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasSuffix(strings.ToLower(line), ".lnk") {
			created = append(created, line)
			ui.Info("created shortcut %s", line)
		}
	}
	if in.Path && err == nil {
		ui.Info("added %s to PATH (applies to newly opened terminals)", in.InstallDir)
	}
	return created, err
}

func removeIntegrations(in integration, files []string) error {
	removeFiles(files)
	script := "Remove-Item -Path " + psq(uninstallKey+in.ID) + " -Recurse -Force -ErrorAction SilentlyContinue\n" +
		pathScript(in.InstallDir, false)
	_, err := powershell(script)
	return err
}

// pathScript adds dir to (or removes it from) the user PATH, keeping
// unexpanded %VARIABLES% intact, and notifies Explorer of the change.
func pathScript(dir string, add bool) string {
	return fmt.Sprintf(`$dir = %s
$add = $%t
$ek = Get-Item -Path 'HKCU:\Environment'
$old = [string]$ek.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
$parts = @($old -split ';' | Where-Object { $_ })
$has = $parts -contains $dir
$change = $false
if ($add -and -not $has) { $parts += $dir; $change = $true }
if (-not $add -and $has) { $parts = @($parts | Where-Object { $_ -ne $dir }); $change = $true }
if ($change) {
  Set-ItemProperty -Path 'HKCU:\Environment' -Name Path -Value ($parts -join ';') -Type ExpandString
  [Environment]::SetEnvironmentVariable('JRUNNER_PATH_REFRESH', '1', 'User')
  [Environment]::SetEnvironmentVariable('JRUNNER_PATH_REFRESH', $null, 'User')
}
`, psq(dir), add)
}

func powershell(script string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	u := utf16.Encode([]rune(script))
	raw := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(raw[2*i:], c)
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-EncodedCommand", base64.StdEncoding.EncodeToString(raw))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("powershell: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// psq quotes s as a PowerShell single-quoted string (PowerShell also treats
// typographic single quotes as quote characters).
func psq(s string) string {
	return "'" + strings.NewReplacer("'", "''", "‘", "‘‘", "’", "’’",
		"‚", "‚‚", "‛", "‛‛").Replace(s) + "'"
}
