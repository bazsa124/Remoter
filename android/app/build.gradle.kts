import java.util.Properties

// The default hub address is the owner's own tailnet name, so it lives in the
// git-ignored local.properties (remoter.hub=https://<hub>.<tailnet>.ts.net),
// not in the source. Without it the app simply asks for the address.
val localProps = Properties().apply {
	rootProject.file("local.properties").takeIf { it.exists() }?.inputStream()?.use { load(it) }
}

plugins {
	alias(libs.plugins.android.application)
	alias(libs.plugins.kotlin.android)
	alias(libs.plugins.kotlin.compose)
	alias(libs.plugins.kotlin.serialization)
}

android {
	namespace = "dev.remoter"
	compileSdk = 35

	defaultConfig {
		applicationId = "dev.remoter"
		minSdk = 29
		targetSdk = 35
		versionCode = 3
		versionName = "0.3.0"
		buildConfigField("String", "DEFAULT_HUB", "\"${localProps.getProperty("remoter.hub", "")}\"")
	}

	buildTypes {
		release {
			// Sideloaded, never published: signing with the debug key keeps the
			// signature identical across builds, so every APK installs as an
			// in-place update of the last.
			signingConfig = signingConfigs.getByName("debug")
			isMinifyEnabled = true
			isShrinkResources = true
			proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
		}
	}

	compileOptions {
		sourceCompatibility = JavaVersion.VERSION_11
		targetCompatibility = JavaVersion.VERSION_11
	}
	kotlinOptions { jvmTarget = "11" }

	buildFeatures {
		compose = true
		buildConfig = true
	}
}

dependencies {
	implementation(libs.androidx.core.ktx)
	implementation(libs.androidx.lifecycle.runtime.ktx)
	implementation(libs.androidx.lifecycle.service)
	implementation(libs.androidx.activity.compose)

	implementation(platform(libs.androidx.compose.bom))
	implementation(libs.androidx.ui)
	implementation(libs.androidx.ui.graphics)
	implementation(libs.androidx.ui.tooling.preview)
	implementation(libs.androidx.material3)
	implementation(libs.androidx.material.icons)

	// The PIN, optionally kept behind the fingerprint (Keystore key that only a
	// biometric unlock can use).
	implementation(libs.androidx.biometric)

	// The phone as a target: a tiny HTTP + WebSocket server the hub dials.
	implementation(libs.nanohttpd)
	implementation(libs.nanohttpd.websocket)

	implementation(libs.okhttp)
	implementation(libs.kotlinx.serialization.json)
	implementation(libs.coil.compose)
}
