// PILAR XXVI 137.B.1 — root Gradle build script for the NeoAnvil Android app.
// SCAFFOLD ONLY — written without Android Studio. Validate by running
// `./gradlew build` from this directory before merging downstream code.

plugins {
    id("com.android.application") version "8.7.0" apply false
    id("org.jetbrains.kotlin.android") version "2.0.21" apply false
    id("org.jetbrains.kotlin.plugin.compose") version "2.0.21" apply false
}

allprojects {
    repositories {
        google()
        mavenCentral()
    }
}

tasks.register<Delete>("clean") {
    delete(rootProject.layout.buildDirectory)
}
