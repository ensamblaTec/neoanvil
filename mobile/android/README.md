# Neo-Mesh Android app — `mobile/android/`

PILAR XXVI 137.B.1 scaffold. Imports `libneo.aar` (built from the `//go:build mobile`-tagged wrappers under `mobile/`) via gomobile bind, and exposes a Compose UI for brain push / pull operations on a Pixel-class device.

## Status

| Task | State |
|------|-------|
| 137.A.1 — gomobile compat audit | ✅ done — `docs/pilar-xxvi-gomobile-compat-audit.md` |
| 137.A.2 — `mobile/` package with build tags | ✅ done — `mobile/{doc,identity,config,identity_test}.go` |
| 137.A.3 — gomobile bind script | ✅ done — `mobile/scripts/build-android.sh` (validated on Linux without SDK; SDK-required path remains for an Android-equipped machine) |
| 137.A.4 — Android emulator smoke test | ⏳ blocked: needs Android SDK + emulator |
| **137.B.1 — Android Studio project scaffold** | ✅ **this directory** |
| 137.B.2 — MainActivity tabs (Sync / Settings / Logs) | ⏳ awaits Android Studio compile validation |
| 137.B.3 — Settings + Android Keystore credentials | ⏳ awaits Android Studio compile validation |
| 137.B.4 — Power policy switch | ⏳ awaits Android Studio compile validation |
| 137.C.* — Foreground Service + JNI + BatteryManager | ⏳ blocked on Android tooling |
| 137.D.* — JobScheduler with constraints | ⏳ blocked on Android tooling |
| 137.E.* — Tailscale on Android | ⏳ blocked on Android tooling |
| 137.F.* — Pixel runtime + Anthropic Custom Connectors | ⏳ blocked on physical Pixel |
| 137.G.* — GitHub webhook in NeoMeshService | ⏳ blocked on Android tooling |

## What's here

```
mobile/android/
├── build.gradle.kts                    root Gradle build (AGP 8.7, Kotlin 2.0.21, Compose plugin)
├── settings.gradle.kts                 module list + repos
├── gradle.properties                   AndroidX, JVM args, parallel + caching
├── README.md                           this file
└── app/
    ├── build.gradle.kts                app module — compileSdk 34, minSdk 28, Compose enabled
    ├── proguard-rules.pro              R8 keep rules for gomobile bridge + Compose preview
    └── src/main/
        ├── AndroidManifest.xml         INTERNET / FOREGROUND_SERVICE / POST_NOTIFICATIONS perms
        ├── res/xml/
        │   └── data_extraction_rules.xml   opt-out of cloud backup + device transfer
        └── java/com/neogo/neo/
            └── MainActivity.kt         Compose entry point — placeholder until 137.B.2
```

The `dependencies` block in `app/build.gradle.kts` has the JNI bridge import commented out:

```kotlin
// implementation(files("../build/libneo.aar"))
```

Re-enable this line after running `mobile/scripts/build-android.sh` on a machine with the Android SDK installed and producing the AAR. Until then the app compiles without the JNI bridge — useful for iterating on Compose UI in 137.B.2-B.4 before 137.A.3 has been validated end-to-end.

## How to validate this scaffold

On a machine with Android Studio Iguana (2024.2) or newer:

1. `File → Open` → pick `mobile/android/`. Let Gradle sync (~2 min on first run).
2. `Build → Make Project`. Should compile without errors.
3. `Run → Run 'app'` on an emulator running API 34. The app shows a "Neo-Mesh — 137.B.1 scaffold" placeholder.

If Gradle sync fails at the platform / NDK download step, install the requirements via `Tools → SDK Manager`:

- Android SDK Platform 34
- Android SDK Build-Tools 34.0.0
- NDK (Side by side) — required only when 137.A.3's `build-android.sh` runs
- Android SDK Command-line Tools (for `gradlew` headless usage)

## What this scaffold deliberately does NOT have

- No Compose UI for the Sync / Settings / Logs tabs (137.B.2). The `ScaffoldPlaceholder()` composable is a 5-line stub.
- No `EncryptedSharedPreferences` wiring (137.B.3). The dependency is in `app/build.gradle.kts` so 137.B.3 doesn't have to touch Gradle.
- No `NeoMeshService` (137.C.1). The manifest has a commented-out `<service>` entry showing where it lands.
- No `JobScheduler` (137.D). No `JobService` subclass yet.
- No JNI bridge (137.A.3 + 137.C.2). The AAR import line is commented out.

These intentional omissions keep the scaffold reviewable and prevent shipping unverified Kotlin that would have to be rewritten when an Android dev validates against actual Android Studio compile feedback. The empty parts are explicit invitations for the next épica's authored work — not mistakes.
