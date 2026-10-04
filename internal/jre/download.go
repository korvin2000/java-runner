package jre

import (
	"context"
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
	url, sha256, name string
}

// Download fetches Java feature release `feature` from the first source
// that works, verifies and unpacks it into dir (replacing its content) and
// checks that it runs on this machine.
func Download(ctx context.Context, sources []config.Source, feature int, req Requirement, dir string) (*Runtime, error) {
	image := "jre"
	if req.JDK {
		image = "jdk"
	}
	var errs []string
	for _, src := range sources {
		ui.Info("source: %s", src.Label())
		a, err := resolve(ctx, src, feature, image)
		if err == nil {
			var rt *Runtime
			if rt, err = install(ctx, a, req, dir); err == nil {
				return rt, nil
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
		return nil, err
	}
	file := dir + ".download"
	defer os.Remove(file)
	ui.Info("downloading %s", a.name)
	if err := fetch.File(ctx, a.url, file, a.sha256); err != nil {
		return nil, err
	}
	if a.sha256 != "" {
		ui.Info("checksum verified")
	} else {
		ui.Warn("no checksum published for this download, integrity not verified")
	}
	ui.Info("unpacking")
	tmp := dir + ".new"
	_ = os.RemoveAll(tmp)
	if err := archive.Extract(file, tmp); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	home, err := FindHome(tmp)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	FixPermissions(home)
	rt, err := ProbeExec(home)
	if err == nil {
		if why := req.Check(rt); why != "" {
			err = fmt.Errorf("downloaded runtime is not usable: %s", why)
		}
	}
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	rel, _ := filepath.Rel(tmp, home)
	if err := fsutil.ReplaceDir(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	rt.Home = filepath.Join(dir, rel)
	rt.Source = "downloaded"
	return rt, nil
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
	}
	return nil, fmt.Errorf("unknown source %q", src.Provider)
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
			return &artifact{url: p.Link, sha256: p.Checksum, name: p.Name}, nil
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
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	q := url.Values{
		"java_version": {strconv.Itoa(feature)}, "os": {osName}, "arch": {arch}, "archive_type": {ext},
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
		a.sha256 = detail.SHA256
	}
	return a, nil
}

// fromURL expands a URL template; sha256 may be a digest or a URL (template)
// of a checksum file whose first token is the digest.
func fromURL(ctx context.Context, src config.Source, feature int, image string) (*artifact, error) {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	expand := strings.NewReplacer("{version}", strconv.Itoa(feature), "{os}", platform.OS(),
		"{arch}", platform.Arch(), "{image}", image, "{ext}", ext).Replace
	a := &artifact{url: expand(src.URL), sha256: src.SHA256}
	if u, err := url.Parse(a.url); err == nil {
		a.name = path.Base(u.Path)
	}
	if strings.Contains(a.sha256, "://") {
		txt, err := fetch.Text(ctx, expand(a.sha256))
		if err != nil {
			return nil, fmt.Errorf("checksum: %w", err)
		}
		if f := strings.Fields(txt); len(f) > 0 {
			a.sha256 = f[0]
		}
	}
	return a, nil
}
