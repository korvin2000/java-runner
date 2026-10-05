package jre

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/korvin2000/java-runner/internal/archive"
	"github.com/korvin2000/java-runner/internal/config"
	"github.com/korvin2000/java-runner/internal/fetch"
	"github.com/korvin2000/java-runner/internal/fsutil"
	"github.com/korvin2000/java-runner/internal/platform"
	"github.com/korvin2000/java-runner/internal/ui"
)

type artifact struct {
	url, sum, name string // sum: hex SHA-256 (or SHA-1 for vendors without SHA-256)
	note           string // e.g. "JDK, this vendor publishes no JRE"
}

// localError is a failure on this computer (e.g. files in use) that another
// download source cannot fix.
type localError struct{ error }

// Download fetches Java feature release `feature` from the first source
// that works, verifies and unpacks it into dir (replacing its content) and
// checks that it runs on this machine.
func Download(ctx context.Context, sources []config.Source, feature int, req Requirement, dir string) (*Runtime, error) {
	image := "jre"
	if req.JDK {
		image = "jdk"
	}
	var errs []string
	for i, src := range sources {
		ui.Info("source %d/%d: %s", i+1, len(sources), src.Label())
		a, err := resolve(ctx, src, feature, image)
		if err == nil {
			var rt *Runtime
			if rt, err = install(ctx, a, req, dir); err == nil {
				return rt, nil
			}
			var le localError
			if errors.As(err, &le) {
				return nil, le.error
			}
		}
		ui.Warn("%v", err)
		errs = append(errs, src.Label()+": "+err.Error())
	}
	return nil, fmt.Errorf("could not download Java %d %s for %s:\n  %s\nInstall Java %s manually or check your network/proxy settings",
		feature, image, platform.Key(), strings.Join(errs, "\n  "), req)
}

func install(ctx context.Context, a *artifact, req Requirement, dir string) (*Runtime, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, localError{err}
	}
	file := dir + ".download"
	defer os.Remove(file)
	ui.Info("downloading %s", a.name)
	if a.note != "" {
		ui.Info("%s", a.note)
	}
	if err := fetch.File(ctx, a.url, file, a.sum); err != nil {
		return nil, err
	}
	if a.sum != "" {
		ui.Success("checksum verified")
	} else {
		ui.Warn("no checksum published for this download, integrity not verified")
	}
	tmp := dir + ".new"
	_ = os.RemoveAll(tmp)
	stop := ui.Spin("unpacking %s", a.name)
	err := archive.Extract(file, tmp)
	var home string
	if err == nil {
		home, err = FindHome(tmp)
	}
	var rt *Runtime
	if err == nil {
		FixPermissions(home)
		if rt, err = ProbeExec(home); err == nil {
			if why := req.Check(rt); why != "" {
				err = fmt.Errorf("downloaded runtime is not usable: %s", why)
			}
		}
	}
	stop(err == nil, "Java "+versionOf(rt)+" works")
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	rel, _ := filepath.Rel(tmp, home)
	if err := fsutil.ReplaceDir(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return nil, localError{fmt.Errorf("cannot replace %s (%v); if the application is running, close it and try again", dir, err)}
	}
	rt.Home = filepath.Join(dir, rel)
	rt.Source = "downloaded"
	return rt, nil
}

func versionOf(rt *Runtime) string {
	if rt == nil {
		return ""
	}
	return rt.Version
}

func resolve(ctx context.Context, src config.Source, feature int, image string) (*artifact, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch {
	case src.URL != "":
		return fromURL(ctx, src, feature, image)
	case src.Provider == "adoptium":
		return adoptium(ctx, feature, image)
	case src.Provider == "zulu":
		return zulu(ctx, feature, image)
	case src.Provider == "corretto":
		return corretto(ctx, feature, image)
	case src.Provider == "liberica":
		return liberica(ctx, feature, image)
	case src.Provider == "microsoft":
		return microsoft(ctx, feature, image)
	}
	return nil, fmt.Errorf("unknown source %q", src.Provider)
}

func archiveExt() string {
	if runtime.GOOS == "windows" {
		return "zip"
	}
	return "tar.gz"
}

// jdkOnly is the note shown when a vendor has no JRE build and a JDK is used.
const jdkOnly = "this vendor publishes no JRE build for this release, using the (larger) JDK"

// checksumFile fetches a "<hex> <name>" checksum file and returns the digest.
func checksumFile(ctx context.Context, u string) (string, error) {
	txt, err := fetch.Text(ctx, u)
	if err != nil {
		return "", fmt.Errorf("checksum: %w", err)
	}
	f := strings.Fields(txt)
	if len(f) == 0 || (len(f[0]) != 64 && len(f[0]) != 40) {
		return "", fmt.Errorf("checksum: %s has no digest", u)
	}
	return f[0], nil
}

// corretto uses Amazon's permanent "latest" download links. Corretto ships a
// JRE only for Java 8.
func corretto(ctx context.Context, feature int, image string) (*artifact, error) {
	osName := map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux"}[runtime.GOOS]
	if platform.IsMusl() {
		osName = "alpine-linux"
	}
	arch := map[string]string{"amd64": "x64", "arm64": "aarch64", "386": "x86"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		return nil, fmt.Errorf("platform %s/%s is not supported by Corretto", runtime.GOOS, runtime.GOARCH)
	}
	pkg, note := "jdk", ""
	if image == "jre" {
		if feature == 8 && runtime.GOOS != "darwin" {
			pkg = "jre"
		} else {
			note = jdkOnly
		}
	}
	name := fmt.Sprintf("amazon-corretto-%d-%s-%s-%s.%s", feature, arch, osName, pkg, archiveExt())
	sum, err := checksumFile(ctx, "https://corretto.aws/downloads/latest_sha256/"+name)
	if err != nil {
		return nil, fmt.Errorf("no Corretto %d for %s (%v)", feature, platform.Key(), err)
	}
	return &artifact{url: "https://corretto.aws/downloads/latest/" + name, sum: sum, name: name, note: note}, nil
}

// liberica uses the BellSoft Liberica API (publishes SHA-1 digests).
func liberica(ctx context.Context, feature int, image string) (*artifact, error) {
	osName := map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux"}[runtime.GOOS]
	if platform.IsMusl() {
		osName = "linux-musl"
	}
	arch, bits := map[string]string{"amd64": "x86", "386": "x86", "arm64": "arm", "arm": "arm"}[runtime.GOARCH], "64"
	if runtime.GOARCH == "386" || runtime.GOARCH == "arm" {
		bits = "32"
	}
	if osName == "" || arch == "" {
		return nil, fmt.Errorf("platform %s/%s is not supported by Liberica", runtime.GOOS, runtime.GOARCH)
	}
	q := url.Values{
		"version-feature": {strconv.Itoa(feature)}, "version-modifier": {"latest"}, "release-type": {"ga"},
		"os": {osName}, "arch": {arch}, "bitness": {bits}, "installation-type": {"archive"},
		"package-type": {archiveExt()}, "bundle-type": {image}, "fx": {"false"},
	}
	var list []struct {
		URL      string `json:"downloadUrl"`
		SHA1     string `json:"sha1"`
		Filename string `json:"filename"`
	}
	if err := fetch.JSON(ctx, "https://api.bell-sw.com/v1/liberica/releases?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	if len(list) == 0 || list[0].URL == "" {
		return nil, fmt.Errorf("no Liberica %d %s build for %s", feature, image, platform.Key())
	}
	return &artifact{url: list[0].URL, sum: list[0].SHA1, name: list[0].Filename}, nil
}

// microsoft uses the Microsoft Build of OpenJDK "latest" links (JDK only,
// Java 11, 17, 21 and newer LTS releases).
func microsoft(ctx context.Context, feature int, image string) (*artifact, error) {
	osName := map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[runtime.GOARCH]
	if osName == "" || arch == "" || platform.IsMusl() {
		return nil, fmt.Errorf("platform %s/%s is not supported by Microsoft OpenJDK", runtime.GOOS, runtime.GOARCH)
	}
	name := fmt.Sprintf("microsoft-jdk-%d-%s-%s.%s", feature, osName, arch, archiveExt())
	u := "https://aka.ms/download-jdk/" + name
	sum, err := checksumFile(ctx, u+".sha256sum.txt")
	if err != nil {
		return nil, fmt.Errorf("no Microsoft OpenJDK %d for %s (%v)", feature, platform.Key(), err)
	}
	a := &artifact{url: u, sum: sum, name: name}
	if image == "jre" {
		a.note = jdkOnly
	}
	return a, nil
}

// adoptium uses the Eclipse Adoptium API (Temurin builds).
func adoptium(ctx context.Context, feature int, image string) (*artifact, error) {
	osName := map[string]string{"windows": "windows", "darwin": "mac", "linux": "linux"}[runtime.GOOS]
	if platform.IsMusl() {
		osName = "alpine-linux"
	}
	arch := map[string]string{"amd64": "x64", "arm64": "aarch64", "386": "x86", "arm": "arm",
		"ppc64le": "ppc64le", "s390x": "s390x", "riscv64": "riscv64"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		return nil, fmt.Errorf("platform %s/%s is not supported by Adoptium", runtime.GOOS, runtime.GOARCH)
	}
	u := fmt.Sprintf("https://api.adoptium.net/v3/assets/latest/%d/hotspot?architecture=%s&image_type=%s&os=%s&vendor=eclipse",
		feature, arch, image, osName)
	var res []struct {
		Binary struct {
			ImageType string `json:"image_type"`
			Package   struct {
				Name     string `json:"name"`
				Link     string `json:"link"`
				Checksum string `json:"checksum"`
			} `json:"package"`
		} `json:"binary"`
	}
	if err := fetch.JSON(ctx, u, &res); err != nil {
		return nil, err
	}
	for _, r := range res {
		if p := r.Binary.Package; p.Link != "" && r.Binary.ImageType == image {
			return &artifact{url: p.Link, sum: p.Checksum, name: p.Name}, nil
		}
	}
	return nil, fmt.Errorf("no Temurin %d %s build for %s", feature, image, platform.Key())
}

// zulu uses the Azul metadata API.
func zulu(ctx context.Context, feature int, image string) (*artifact, error) {
	osName := map[string]string{"windows": "windows", "darwin": "macos", "linux": "linux-glibc"}[runtime.GOOS]
	if platform.IsMusl() {
		osName = "linux-musl"
	}
	arch := map[string]string{"amd64": "x64", "arm64": "aarch64", "386": "i686", "arm": "arm"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		return nil, fmt.Errorf("platform %s/%s is not supported by Azul", runtime.GOOS, runtime.GOARCH)
	}
	q := url.Values{
		"java_version": {strconv.Itoa(feature)}, "os": {osName}, "arch": {arch}, "archive_type": {archiveExt()},
		"java_package_type": {image}, "javafx_bundled": {"false"}, "crac_supported": {"false"},
		"latest": {"true"}, "release_status": {"ga"}, "availability_types": {"CA"},
		"page": {"1"}, "page_size": {"1"},
	}
	const api = "https://api.azul.com/metadata/v1/zulu/packages/"
	var list []struct {
		UUID string `json:"package_uuid"`
		Name string `json:"name"`
		URL  string `json:"download_url"`
	}
	if err := fetch.JSON(ctx, api+"?"+q.Encode(), &list); err != nil {
		return nil, err
	}
	if len(list) == 0 || list[0].URL == "" {
		return nil, fmt.Errorf("no Zulu %d %s build for %s", feature, image, platform.Key())
	}
	a := &artifact{url: list[0].URL, name: list[0].Name}
	var detail struct {
		SHA256 string `json:"sha256_hash"`
	}
	if err := fetch.JSON(ctx, api+url.PathEscape(list[0].UUID), &detail); err == nil {
		a.sum = detail.SHA256
	}
	return a, nil
}

// fromURL expands a URL template; sha256 may be a digest or a URL (template)
// of a checksum file whose first token is the digest.
func fromURL(ctx context.Context, src config.Source, feature int, image string) (*artifact, error) {
	expand := strings.NewReplacer("{version}", strconv.Itoa(feature), "{os}", platform.OS(),
		"{arch}", platform.Arch(), "{image}", image, "{ext}", archiveExt()).Replace
	a := &artifact{url: expand(src.URL), sum: src.SHA256}
	if u, err := url.Parse(a.url); err == nil {
		a.name = path.Base(u.Path)
	}
	if strings.Contains(a.sum, "://") {
		sum, err := checksumFile(ctx, expand(a.sum))
		if err != nil {
			return nil, err
		}
		a.sum = sum
	}
	return a, nil
}
