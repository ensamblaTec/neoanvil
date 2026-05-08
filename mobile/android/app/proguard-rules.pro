# PILAR XXVI 137.B.1 — release-build R8 / ProGuard rules.
#
# gomobile-generated bridge classes use reflection internally; preserve
# the entire com.neogo.neo namespace AND the gomobile output package.
# Once 137.A.3 produces libneo.aar, the actual generated package will
# typically be `mobile` (the Go package name). Adjust the second -keep
# rule below if `gomobile bind` decides on a different output prefix.

-keep class com.neogo.neo.** { *; }
-keep class mobile.** { *; }

# Compose tooling preview wrappers — reference reflection inside
# androidx.compose.ui.tooling.preview.Preview annotations.
-keep @androidx.compose.ui.tooling.preview.Preview class * { *; }

# Keep Kotlin metadata so reflection over data classes used by JNI bridge
# doesn't blow up at runtime under release builds.
-keep class kotlin.Metadata { *; }
