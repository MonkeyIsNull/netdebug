# Screenshots

The main [README](../../README.md) references **`dashboard.png`** in this
directory as the dashboard image. That file is intentionally not committed yet —
it should be captured from a running dashboard.

## How to capture `dashboard.png`

1. Start the dashboard, ideally with synthetic / sample data so no real SSID,
   IP, or MAC leaks into the image:

   ```sh
   netdebug --serve --ssid "MyNetwork"
   ```

   Let it run for a minute or two so the LIVE and HISTORY charts have data.

2. Open http://127.0.0.1:8099/ and take a screenshot of the page (Cmd-Shift-4,
   then Space to grab the window).

3. **Sanitize before committing.** Confirm no real network name, IP, BSSID, or
   personal hostname is visible. Use `--ssid "MyNetwork"` to label the network,
   and crop/redact anything else identifying.

4. Save it here as `dashboard.png`.

Until then the main README degrades gracefully: the `<img>` shows its alt text
and a note points here.
