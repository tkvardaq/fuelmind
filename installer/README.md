# FuelMind Windows Installer (WiX)

This directory contains the WiX source for building a Windows MSI installer for FuelMind.

## Prerequisites
- WiX Toolset v3.11 or later (https://wixtoolset.org/)
- Built FuelMind binaries (`fuelmind-core.exe` and `fuelmind-launcher.exe`) from the `cmd` directories.

## Building the MSI
1. Ensure the binaries are built and placed in their default output locations:
   - `..\cmd\fuelmind-core\fuelmind-core.exe`
   - `..\cmd\fuelmind-launcher\fuelmind-launcher.exe`
2. Optionally run `heat` to harvest files automatically, but the current `product.wxs` uses explicit file references.
3. From this directory, run:
   ```cmd
   candle product.wxs
   light product.wixobj -out FuelMind-Installer.msi
   ```
4. The resulting MSI can be deployed via standard Windows installation mechanisms.

## Installer Features
- Installs `fuelmind-core.exe` and `fuelmind-launcher.exe` to `Program Files\FuelMind`.
- Installs a Windows Service named **FuelMind Service** that runs `fuelmind-launcher.exe` under the `NT AUTHORITY\LocalService` account.
- Creates a data folder under `%LOCALAPPDATA%\FuelMind` for local state.
- Includes a placeholder for a first-run wizard (to be implemented as a separate executable launched via a custom action after install).

## Next Steps for Phase 8
- Implement the first-run wizard (PIN setup, POS connection test, hardware tier display, test heartbeat button) as a standalone executable (e.g., `fuelmind-setup.exe`).
- Add a custom action in `product.wxs` to launch the wizard after install.
- Refine service configuration to ensure proper start type and recovery options.
- Add upgrade and uninstall logic.
- Localize and add EULA if needed.

## Notes
- The current `product.wxs` uses hard-coded relative paths; consider using HeatDirectory to automate file harvesting.
- Ensure the service binary (`fuelmind-launcher.exe`) is designed to run as a service (handles start/stop commands).
- The installer currently sets the service account to `[LocalSystem]`; change to `LocalService` as required.