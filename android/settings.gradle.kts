pluginManagement {
	repositories {
		google()
		mavenCentral()
		gradlePluginPortal()
	}
}

dependencyResolutionManagement {
	repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
	repositories {
		google()
		mavenCentral()
		// Termux publishes its terminal widgets here, not to Maven Central.
		maven { url = uri("https://jitpack.io") }
	}
}

rootProject.name = "Remoter"
include(":app")
