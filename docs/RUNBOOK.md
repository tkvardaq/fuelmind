# FuelMind Runbook (solo-founder on-call)

> Phase 0 placeholder. Real content lands in Phase 8 (Pilot prep). For
> now, this file exists so the path is correct and the table of contents
> below is what you'll be writing toward.

## When an owner calls

0. **Stay calm.** Phase 0–9: the local core is in your hands. Phase 6+:
   most issues can be fixed by the unattended update mechanism without
   you touching the shop PC.
1. Ask: *"What does your dashboard show, and what does the POS show?"*
2. Ask: *"Is your internet up at the station?"*
3. Ask: *"When did this start — was there a power cut, a POS update,
   a FuelMind update?"*
4. Tell them: *"I'm going to remote in / I'll be there in N hours."*

## Common scenarios (to be filled in)

- "Dashboard says zero sales but the POS has sales" → check
  `raw_pos_transactions` row count, check the adapter's last-pull log
- "Dashboard shows wrong diesel total" → check `product_aliases` for
  the vendor's spelling of "diesel"
- "I can't log in" → PIN reset flow (Phase 8)
- "Dashboard is blank" → check service status, check
  `C:\ProgramData\FuelMind\logs\`, check disk space
- "Numbers don't match yesterday" → check that no fuel was delivered
  overnight (delivery updates the inventory but not sales; mismatch
  may be a delivery entry missed)

## Manual install / re-install (Phase 8)

- Installer location: `installer/fuelmind-1.0.0.msi`
- Pre-flight: shop PC is Windows 10/11 64-bit, 4GB+ RAM free
- First-run wizard: PIN, POS connection test, hardware tier, test
  heartbeat button
- Verify: cloud admin shows the station green within 30 min

## Rollback an update (Phase 6)

- Trigger: update applied but self-check failed
- Automatic: local core rolls back to previous binary within 60s,
  reports failure in next heartbeat
- Manual (if automatic rollback also failed): RDP into the shop PC,
  run `sc stop FuelMind`, swap `C:\Program Files\FuelMind\fuelmind-core.exe`
  with the copy in `previous/`, run `sc start FuelMind`

## Disaster recovery

- Local DB corrupted: `C:\ProgramData\FuelMind\backups\` has last 7
  nights. Restore the most recent good one, run installer, set PIN.
- Shop PC replaced: install FuelMind on new PC, authenticate with
  license key (printed on first install), if cloud backup exists and
  the customer provides the encryption passphrase, offer restore.
  Otherwise: start fresh, re-sync from the POS (most POSes retain
  at least 30 days of history).
