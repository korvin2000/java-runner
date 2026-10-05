# jrunner

**jrunner** turns a Java application (a fat jar, a Spring Boot jar or a modular
app) into a native executable for Windows, Linux and macOS. The executable
installs itself, finds a suitable Java runtime or downloads one, keeps the app
up to date, and starts it quickly. It fills the same niche as launch4j
and install4j, with a narrower feature set. It is written in Go, has
no dependencies beyond the standard library, and needs nothing on the user's
machine.

```
jrunner init target/demo-app.jar      # writes jrunner.json (detects Spring Boot, port, Java version)
jrunner build --target all            # dist/demo-app-1.0.0-windows-x64.exe, -linux-x64, -mac-aarch64, ...
```

The user downloads one file and runs it. The first start looks like this:

```
Demo App 1.0.0  first launch, installing
  location: C:\Users\anna\AppData\Local\Programs\Demo App
  ✓ application files (48.2 MB)
  ✓ created shortcut C:\Users\anna\Desktop\Demo App.lnk
  ✓ installed Demo App 1.0.0
▸ Looking for Java 17+
  no suitable Java installation found on this computer
▸ Downloading Java 21 (jre, windows-x64)
  source 1/5: Eclipse Temurin (adoptium.net)
  downloading OpenJDK21U-jre_x64_windows_hotspot_21.0.4_7.zip
  [████████████████████████] 100%  44.1 MB / 44.1 MB  9.8 MB/s
  ✓ downloaded 44.1 MB (4s)
  ✓ checksum verified
  ✓ Java 21.0.4+7-LTS works (2s)
  ✓ installed Java 21.0.4+7-LTS (Eclipse Adoptium) at ...\runtime\jdk-21.0.4+7-jre
▸ Starting Demo App
  ... application log ...
▸ Demo App is ready at http://localhost:8080/
```

Every later start prints nothing of its own and takes a few milliseconds
before the JVM runs. Ready-made configurations for typical cases are in
[`examples/`](examples/README.md).

## How it works

```
First launch (only once)                      Every later launch
──────────────────────────────────────────    ─────────────────────────────────
1. install app into the program folder        1. read state.json
   (+ shortcut / menu entry / PATH)           2. check Java still exists (1 stat)
2. find Java >= minVersion                    3. (every 24h) check for an update
   JAVA_HOME, PATH, well-known JDK dirs       4. start the app
3. none found? download a JRE                    (web app: wait for port, open browser)
   Adoptium -> Azul Zulu -> your mirrors,
   verify SHA-256, unpack, test-run it
4. remember everything in state.json
5. start the app
```

* **One binary, two roles.** `jrunner` without an embedded application is the
  developer CLI. `jrunner build` appends an application package (a zip with
  `jrunner.json`, the jar and optionally a jlink runtime) to a copy of the
  jrunner binary for the target platform. That copy is the app's launcher.
  With `--thin` the package contains only `jrunner.json`: such a launcher
  (about 7 MB) downloads the application from `update.url` on first start.
* **Per-user install, no admin rights.** Nothing is written outside the user profile.

  | | Program files | Data / working dir | Integration |
  |---|---|---|---|
  | Windows | `%LOCALAPPDATA%\Programs\<Name>` | `%LOCALAPPDATA%\<Name>` | Start menu + desktop `.lnk`, *Settings → Apps* entry with uninstall, user `PATH` |
  | macOS | `~/Applications/<Name>` | `~/Library/Application Support/<Name>` | `~/Applications/<Name>.app`, `~/Desktop/<Name>.command`, `~/.local/bin` link |
  | Linux | `~/.local/opt/<id>` | `~/.local/share/<id>` | `.desktop` menu + desktop entry, `~/.local/bin` link |

  Install layout: `<id>[.exe]` (launcher), `app/` (jar + files + `jrunner.json`),
  `runtime/` (downloaded or bundled Java), `state.json`.
* **Fast and reliable later launches.** Discovery results are stored in
  `state.json`. On Unix the launcher `exec`s Java, so no extra process stays
  around. If the remembered Java disappears, discovery runs again. If
  `state.json` is damaged or the app files are missing, the app is reinstalled.
  Installs are atomic (`app.new` → `app`), and concurrent first launches are
  serialized with a lock file.
* **Upgrades and downgrades.** Running a newer installer upgrades the
  installation. Running an older one (e.g. the file still in *Downloads*)
  starts the newer installed version and never downgrades it.

## Configuration (`jrunner.json`)

Paths are relative to the configuration file. Only `name`, `version` and
`jar` (or `java.mainClass` / `java.module`) are required. Unknown keys are
reported as errors, so typos are caught at build time.

```json
{
  "name": "Demo App",
  "id": "demo-app",
  "version": "1.2.0",
  "publisher": "ACME Inc.",
  "jar": "target/demo-app-1.2.0.jar",
  "files": ["config/*.yml", "lib"],
  "icons": ["assets/app.ico", "assets/app.icns", "assets/app.png"],
  "workDir": "${DATA_DIR}",

  "java": {
    "minVersion": 17,
    "maxVersion": 0,
    "image": "jre",
    "downloadVersion": 21,
    "download": ["adoptium", "zulu", "https://mirror.example.com/jre-{version}-{os}-{arch}.{ext}"],
    "options": ["-Xmx512m", "-Dlogging.file.name=${DATA_DIR}/logs/app.log"],
    "env": { "SPRING_PROFILES_ACTIVE": "prod" },
    "args": []
  },

  "browser": { "url": "http://localhost:8080/", "timeout": 120 },   // or {} to detect the URL from the log

  "update": { "url": "https://example.com/demo-app/update.json", "intervalHours": 24, "auto": false },

  "install": { "desktopShortcut": true, "menuShortcut": true, "addToPath": false }
}
```

| Key | Meaning |
|---|---|
| `id` | File-system name (derived from `name`): Linux dirs, launcher and command name, `<ID>_JAVA_OPTS` |
| `files` | Extra files, directories or globs. They are copied into `app/` and keep their path relative to the config file |
| `icons` | `.ico` is used on Windows, `.icns` on macOS, `.png`/`.svg` on Linux |
| `workDir` | Working directory of the app (default: per-user data dir) |
| `java.minVersion` / `maxVersion` | Accepted feature releases (default 17 / any) |
| `java.image` | `jre` (default) or `jdk` (requires `javac`, downloads a JDK) |
| `java.downloadVersion` | Release to download if nothing suitable is installed (default `minVersion`) |
| `java.download` | Sources, tried in order until one delivers a working runtime (see *Java runtimes* below). A URL template may use `{version} {os} {arch} {image} {ext}` (`os` = windows/linux/mac, `arch` = x64/aarch64, `ext` = zip on Windows, else tar.gz). Object form: `{"url": "...", "sha256": "<hex or URL of a checksum file>"}`. Default: all five built-in providers |
| `java.runtime` | Build time: a jlink image to bundle (may contain `{os}`, `{arch}`, `{platform}`). The app then never searches for or downloads Java |
| `java.options` / `args` | JVM options and default app arguments. `${APP_DIR}`, `${INSTALL_DIR}`, `${DATA_DIR}`, `${CWD}`, `${HOME}`, `${VERSION}`, `${ID}` and `${ENV_VAR}` are expanded |
| `java.env` | Environment variables set for the application (same `${...}` expansion) |
| `java.mainClass` + `classPath` | Start a class instead of `-jar` (entries relative to `app/`, `lib/*` allowed) |
| `java.module` + `modulePath` | Modular app: `-p <modulePath> -m <module>/<main class>` |
| `browser.url` | Web app (e.g. Spring Boot): after start, wait until the port accepts connections, then open the browser. If the port is already in use, the app is assumed to be running and only the browser is opened. Leave the URL out (`"browser": {}`) to detect it from the application's log (see below) |
| `update.url` | `update.json` location. Checks are rate-limited by `intervalHours` (0 = every launch), time out after 10 s and never block the app when offline. `auto: true` installs without asking |
| `install.dir` | Override the install directory |

## Java runtimes

The launcher first looks for an installed Java that matches `java.minVersion`
/ `maxVersion`, the CPU architecture and (for `"image": "jdk"`) has a compiler:
its own `runtime/` directory, `<ID>_JAVA_HOME`, `JAVA_HOME`, `java` on the
`PATH` (shims such as Oracle's `javapath` are resolved), then the usual
installation directories of all major vendors, SDKMAN, IntelliJ's `~/.jdks`
and Homebrew. The newest acceptable release wins.

If nothing fits, `java.downloadVersion` is downloaded. The sources are tried
in order; a source that has no build for this release or platform, is
unreachable, delivers a bad checksum or a runtime that does not start is
skipped with a warning and the next one is tried:

| Source | Builds | Checksum | Notes |
|---|---|---|---|
| `adoptium` | Eclipse Temurin JRE + JDK, 8, 11, 17, 21, 25, ... | SHA-256 | Alpine/musl supported |
| `zulu` | Azul Zulu JRE + JDK, every release incl. 9–16 | SHA-256 | Alpine/musl supported |
| `corretto` | Amazon Corretto JDK (JRE only for 8), 8, 11, 17, 21, ... | SHA-256 | Alpine/musl supported |
| `liberica` | BellSoft Liberica JRE + JDK, 8, 11, 17, 21, ... | SHA-1 | Alpine/musl supported |
| `microsoft` | Microsoft Build of OpenJDK, JDK only, 11, 17, 21 | SHA-256 | |
| URL template | your own mirror or minified JRE (`zip` / `tar.gz` with `bin/java`) | optional | |

When a `jre` is requested and the vendor publishes only JDKs, the JDK is used
(it is larger but equivalent). The official OpenJDK builds on jdk.java.net
have no stable download addresses, so they are not a built-in source; Temurin
is the reference build most distributions ship. The downloaded runtime is
unpacked into `<install dir>/runtime`, test-run and remembered in `state.json`;
it is downloaded once.

Downloads (runtimes, update packages, thin-launcher installs) survive bad
connections: failed transfers are retried with backoff, an interrupted
download is resumed where it stopped (also by the next start, for up to three
days), a corrupted one (checksum mismatch) is downloaded again, and a complete
download that could not be installed (disk full, files in use) is reused. A
private runtime that was damaged later is detected and replaced. Problems on
this computer (no write permission, disk full, a `noexec` mount) are reported
directly instead of trying every other source.

## Web applications and the browser

With `"browser": {"url": "http://localhost:8080/"}` the launcher starts the
JVM, polls the port and opens the URL in the default browser when it accepts
connections. Slow starts are fine (large Spring Boot apps may need minutes):
the launcher keeps waiting as long as the JVM runs; after `timeout` seconds
(default 120) it only prints a hint that the app is still starting.

With `"browser": {}` the launcher reads the application's own output and opens
the first local address it announces, for example:

```
Tomcat started on port 8080 (http) with context path '/shop'   → http://localhost:8080/shop/
Netty started on port 9000                                     → http://localhost:9000/
Studio is ready: http://127.0.0.1:8740/launch?t=0233d4...      → that URL
  ➜  started on Local:   http://localhost:4200/                → http://localhost:4200/
```

Only addresses on this machine (`localhost`, `127.x`, `0.0.0.0`, `::1`, the
host name) are considered, so links to documentation in log messages are
ignored. This also works with `server.port=0` (random port). The launcher
still waits for the port to accept connections before opening the browser.
The wait is not limited while the application runs; `timeout` counts from its
latest output line, so an app that keeps logging during a long startup is not
cut short, and a URL announced late still opens the browser.
`jrunner init` configures this automatically for Spring Boot jars.
`--jrunner-no-browser` starts the app without opening anything.

## Thin launchers

`jrunner build --thin` builds launchers that contain only the configuration
(`dist/<id>-<version>-<target>-thin[.exe]`). On first start such a launcher
downloads the application package from `update.url`, verifies it and installs
it like the full installer would; afterwards it behaves exactly like a normal
launcher (update checks included). Use it when the download page should always
hand out the latest version, or when the application is large and changes
often while the launcher stays the same. The update packages and `update.json`
are written by the same `jrunner build` run.

## Launcher options

All arguments are passed to the application except these:

```
--jrunner-update      check for an update now and install it
--jrunner-reinstall   reinstall from this launcher and look for Java again
--jrunner-uninstall   remove the app, shortcuts and PATH entry (user data is kept)
--jrunner-info        show versions, paths, the Java in use, update status
--jrunner-no-browser  do not open the browser (web applications)
--jrunner-verbose     diagnostic output (also: JRUNNER_VERBOSE=1)
--jrunner-help
```

Users can tune the JVM without touching the configuration:

* `<install dir>/<id>.vmoptions` – one JVM option per line (`#` comments), kept
  across updates, e.g. `-Xmx2g`.
* `<ID>_JAVA_OPTS` – extra JVM options, e.g. `DEMO_APP_JAVA_OPTS="-Xmx2g"`.
* `<ID>_JAVA_HOME` – use this Java installation for the launch instead of the
  discovered one (it is checked against the version requirement, not remembered).

## Updates

When `update` is configured, `jrunner build` also writes `dist/update.json`
and `dist/<id>-<version>.zip` (the same package as inside the launcher). Upload
both next to each other at `update.url`:

```json
{ "version": "1.3.0", "url": "demo-app-1.3.0.zip", "sha256": "…", "notes": "Faster startup" }
```

The launcher compares versions (semver-like: `1.10.0 > 1.9.2`, `2.0.0-rc1 < 2.0.0`),
downloads the package, verifies its SHA-256, checks that the package really
contains the announced version of this application and atomically replaces
`app/`. If the new version needs a newer Java, it is found or downloaded
automatically. Files in `app/` are replaced on update; keep user-editable
configuration in `${DATA_DIR}` (the default working directory).

In CI, pass the release version instead of editing the file:
`jrunner build --target all --version 1.3.0`. Use an `https://` update URL;
with plain `http://` the checksums in `update.json` can be tampered with.
With a bundled runtime, packages are per platform (`"platforms": {"windows-x64": {...}}`).

## Minimal runtimes (jlink)

```
jrunner jlink --jar target/app.jar --out runtime          # jdeps detects the modules
# then in jrunner.json:  "java": { "runtime": "runtime" }
```

For Spring Boot jars the nested `BOOT-INF/lib` jars are analysed too. If jdeps
fails, a module set that fits typical Spring Boot apps is used.
`jdk.crypto.ec` is added for TLS. Images are typically 40–60 MB instead of
about 130 MB for a full JRE. To build images for other platforms, pass the
target JDK's jmods: `--jmods /path/to/jdk-21-windows/jmods --out runtimes/windows-x64`
and use `"runtime": "runtimes/{platform}"`.

## Building jrunner

```
make            # dist/jrunner (current platform) + dist/stubs/jrunner-<target> for all 6 targets
```

`jrunner build --target all` takes the stub for the current platform from
itself, and stubs for other targets from `--stubs DIR`, `<jrunner dir>/stubs`
or `<jrunner dir>`. Ship `jrunner` together with its `stubs/` directory.
Requires Go 1.22+.

## Notes and limitations

* **Console.** Launchers are console programs, so progress and app logs are visible.
  Desktop and menu shortcuts open a terminal on Linux and macOS. On Windows, a
  launcher started by double-click keeps its window open after an error until
  Enter is pressed. Colors and Unicode symbols are used on terminals that
  support them (Windows Terminal, macOS Terminal, Linux); `NO_COLOR=1` turns
  colors off. In URL-detection mode the application's output passes through the
  launcher; Spring Boot keeps its log colors (`SPRING_OUTPUT_ANSI_ENABLED` is
  set), other frameworks may switch them off because stdout is a pipe.
* **Code signing.** The payload is appended to the executable. Windows
  Authenticode signing of the final `.exe` is supported, because the launcher finds
  its payload in front of the certificate table. macOS binaries are ad-hoc
  signed by the Go linker and run with the appended payload. Notarization of
  such a binary is not supported. Unsigned downloads must be allowed once
  (Windows SmartScreen *More info → Run anyway*, macOS right-click → *Open*).
* **No exe icon.** Shortcuts use the configured icon, but the Windows `.exe`
  file keeps the default icon.
* **Update integrity.** Update packages are verified by SHA-256 from
  `update.json`. Serve both over HTTPS. There is no signature verification.
* **Proxies.** Downloads honour `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY`. If
  none is set, the system proxy is used (Windows Internet Options, macOS
  network settings); automatic configuration scripts (PAC) are not supported.
