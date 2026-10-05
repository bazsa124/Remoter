package dev.remoter

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.material3.Surface
import androidx.compose.ui.Modifier
import androidx.core.content.ContextCompat
import androidx.core.view.WindowCompat
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.WindowInsetsControllerCompat
import androidx.fragment.app.FragmentActivity
import dev.remoter.net.HubClient
import dev.remoter.net.HubSettings
import dev.remoter.net.PinVault
import dev.remoter.service.AgentService
import dev.remoter.ui.RemoterApp
import dev.remoter.ui.RemoterTheme

// A FragmentActivity rather than a plain ComponentActivity: the fingerprint
// prompt that releases a saved PIN is a fragment.
class MainActivity : FragmentActivity() {

	private val notificationPermission =
		registerForActivityResult(ActivityResultContracts.RequestPermission()) { /* status only */ }

	override fun onCreate(savedInstanceState: Bundle?) {
		super.onCreate(savedInstanceState)
		WindowCompat.setDecorFitsSystemWindows(window, true)

		val settings = HubSettings(this)
		val client = HubClient(settings)
		val vault = PinVault(this)

		// The listener must be running whenever the owner has turned it on - also
		// after an update, before the boot receiver has had a reason to fire.
		if (dev.remoter.node.NodeSettings(this).enabled) dev.remoter.node.NodeService.start(this)

		setContent {
			RemoterTheme {
				Surface(modifier = Modifier.fillMaxSize()) {
					RemoterApp(
						settings = settings,
						client = client,
						vault = vault,
						activity = this,
						onDevice = { startAgent() },
						onStop = { AgentService.stop(this) },
						onFullscreen = ::applyImmersive,
					)
				}
			}
		}
	}

	/**
	 * Hides the status and navigation bars for a tier that asked for the screen.
	 *
	 * Swipe still reveals them transiently, which is what you want: a remote
	 * desktop should not be able to trap you in it.
	 */
	private fun applyImmersive(on: Boolean) {
		val controller = WindowCompat.getInsetsController(window, window.decorView)
		if (on) {
			WindowCompat.setDecorFitsSystemWindows(window, false)
			controller.hide(WindowInsetsCompat.Type.systemBars())
			controller.systemBarsBehavior =
				WindowInsetsControllerCompat.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE
		} else {
			WindowCompat.setDecorFitsSystemWindows(window, true)
			controller.show(WindowInsetsCompat.Type.systemBars())
		}
	}

	private fun startAgent() {
		// The service is the only thing that keeps the Tier 1 socket alive in the
		// background, and it needs a notification to be allowed to.
		if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
			ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) !=
			PackageManager.PERMISSION_GRANTED
		) {
			notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
		}
		AgentService.start(this)
	}
}
