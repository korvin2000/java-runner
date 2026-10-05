# Example configurations

Copy the `jrunner.json` that is closest to your application next to your
project, adjust `name`, `version` and `jar`, then run `jrunner build --target all`.

| Directory | Scenario |
|---|---|
| [`spring-boot-web/`](spring-boot-web/jrunner.json) | Spring Boot web application. The browser URL is **detected from the log** (`"browser": {}`), logs and the H2 database live in the per-user data directory, updates are checked daily |
| [`cli-tool/`](cli-tool/jrunner.json) | Command-line tool: runs in the caller's directory (`${CWD}`), is put on the `PATH`, accepts Java 11–21 and downloads from three vendors in order |
| [`desktop-jlink/`](desktop-jlink/jrunner.json) | Desktop application with a **bundled jlink runtime** per platform, a `lib/` class path, a splash screen and silent automatic updates |
| [`thin-launcher/`](thin-launcher/jrunner.json) | Small launcher (`jrunner build --thin`) that **downloads the application** from `update.url` on first start and always runs the latest published version |
| [`private-jre/`](private-jre/jrunner.json) | Exactly Java 17 from a **company mirror** (URL template with a checksum file), Eclipse Temurin as fallback |

Placeholders available in `java.options`, `java.args`, `java.env` and `workDir`:
`${APP_DIR}` (installed application files), `${INSTALL_DIR}`, `${DATA_DIR}`
(per-user data, default working directory), `${CWD}` (directory the launcher
was started from), `${HOME}`, `${VERSION}`, `${ID}` and any environment variable.
Build-time placeholders in `java.runtime`: `{os}` (`windows`, `linux`, `mac`),
`{arch}` (`x64`, `aarch64`), `{platform}` (`windows-x64`, ...).
