# FuelMind installer (WiX v3)

## Build

```powershell
powershell -ExecutionPolicy Bypass -File installer\build.ps1 -Version 1.1.0
```

That compiles the binaries into `dist\` first, so the MSI can never ship
a stale executable, then produces:

- `dist\FuelMind-<version>.msi`
- `dist\fuelmind-core-<version>.zip` and `.sha256` — the artifact for the
  update endpoint

Requirements: Go, and WiX Toolset v3 (the script looks in `%WIX%\bin`,
then `%LOCALAPPDATA%\WiX\tools`).

## What the MSI does

- Installs `fuelmind-launcher.exe`, `FuelMindCore.exe` and
  `fuelmind-setup.exe` into `%ProgramFiles%\FuelMind\`.
- Registers the service **FuelMindService** running
  `fuelmind-launcher.exe` as `NT AUTHORITY\LocalService`, automatic
  start, with restart-on-failure recovery.
- Creates `%ProgramData%FuelMind` and its `pos_drop` sub-folder.
  The **service** locks the data folder down on every start
  (LocalService, SYSTEM and Administrators only, inheritance
  broken), while leaving `pos_drop` writable by authenticated users
  so the POS export can deliver files. Doing it in the service
  rather than the installer means a folder restored from backup or
  copied to another PC is protected too.
- Adds a firewall rule for TCP 8765, scoped to the local subnet, so the
  owner's phone can open the dashboard.
- Opens `http://localhost:8765/` after installing, where the owner sets
  the first PIN (only possible from the shop PC).

On first start the launcher copies the installed `FuelMindCore.exe` into
`%ProgramData%\FuelMind\versions\<version>\` and points `current.json` at
it. A later MSI with a newer core is adopted the same way, keeping the
old version as the rollback target.

## Install and uninstall

```powershell
msiexec /i dist\FuelMind-1.1.0.msi /qb        # install
msiexec /i dist\FuelMind-1.1.0.msi /qn        # silent
msiexec /x dist\FuelMind-1.1.0.msi /qb        # uninstall (keeps the data folder)
```

Verify:

```powershell
sc query FuelMindService
"C:\Program Files\FuelMind\fuelmind-setup.exe" status
```

## Notes

- `ProductVersion` comes from `-Version`; ship a new version for every
  build so upgrades replace the old install.
- The data folder is deliberately left behind on uninstall: it holds the
  station's sales history. Delete it by hand to remove everything.

**Support note:** because the data folder is locked down, run
`fuelmind-setup.exe` from an **administrator** prompt; an ordinary account
cannot read the database.
