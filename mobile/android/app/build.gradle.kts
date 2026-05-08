// PILAR XXVI 137.B.1 — Neo-Mesh Android app module.
// SCAFFOLD ONLY — Compose UI implementation lands in 137.B.2-B.4.
// Validate this file with `./gradlew :app:assembleDebug` from mobile/android/.

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

android {
    namespace = "com.neogo.neo"
    compileSdk = 34

    defaultConfig {
        applicationId = "com.neogo.neo"
        minSdk = 28              // Android 9 — covers ~95% of active devices
        targetSdk = 34           // Android 14 — gomobile bind requirement
        versionCode = 1
        versionName = "0.1.0-scaffold"

        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
        }
        debug {
            // Enable Compose live edit + debugger.
            isDebuggable = true
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    buildFeatures {
        compose = true
    }

    // libneo.aar produced by mobile/scripts/build-android.sh (137.A.3).
    // Until 137.A.3 has been run on a machine with the Android SDK + NDK,
    // this directory is empty and the implementation block below is a
    // no-op. The release build will fail loudly until the AAR is present.
    sourceSets["main"].jniLibs.srcDirs("../../build/libs")
}

dependencies {
    // Kotlin stdlib (BOM keeps versions consistent across modules).
    implementation(platform("org.jetbrains.kotlin:kotlin-bom:2.0.21"))

    // AndroidX core + lifecycle.
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.6")
    implementation("androidx.activity:activity-compose:1.9.3")

    // Jetpack Compose (BOM pins all compose-* artifacts to the same version).
    val composeBom = platform("androidx.compose:compose-bom:2024.10.00")
    implementation(composeBom)
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.ui:ui-tooling-preview")
    implementation("androidx.compose.material3:material3")

    // EncryptedSharedPreferences for credential storage (137.B.3).
    implementation("androidx.security:security-crypto:1.1.0-alpha06")

    // libneo.aar — produced by gomobile bind. Until 137.A.3 lands on a
    // dev machine with the SDK installed, the file under ../build/libs/
    // does not exist and Gradle will fail at sync. Comment-out this
    // line to compile the Compose UI without the JNI bridge during
    // initial 137.B.* scaffolding.
    // implementation(files("../build/libneo.aar"))

    // Test
    testImplementation("junit:junit:4.13.2")
    androidTestImplementation("androidx.test.ext:junit:1.2.1")
    androidTestImplementation("androidx.test.espresso:espresso-core:3.6.1")
    androidTestImplementation(composeBom)
    androidTestImplementation("androidx.compose.ui:ui-test-junit4")
    debugImplementation("androidx.compose.ui:ui-tooling")
    debugImplementation("androidx.compose.ui:ui-test-manifest")
}
