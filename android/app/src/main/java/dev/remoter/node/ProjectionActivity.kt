package dev.remoter.node

import android.media.projection.MediaProjectionConfig
import android.media.projection.MediaProjectionManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.result.contract.ActivityResultContracts

/**
 * Shows Android's "Start recording or casting?" prompt for smooth Live, and
 * hands the answer to the node service. Invisible itself: the system dialog is
 * the whole UI.
 *
 * Someone at the phone has to tap "Start now". That is Android's rule for screen
 * recording, and the reason smooth mode is attended while the default is not.
 */
class ProjectionActivity : ComponentActivity() {

	private val consent = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
		NodeService.instance?.projectionResult(result.resultCode, result.data)
		finish()
	}

	override fun onCreate(savedInstanceState: Bundle?) {
		super.onCreate(savedInstanceState)
		if (savedInstanceState != null) return // recreated mid-prompt: the result still arrives
		val mpm = getSystemService(MediaProjectionManager::class.java)
		val intent = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
			// Remote control needs the whole screen; do not offer "a single app".
			mpm.createScreenCaptureIntent(MediaProjectionConfig.createConfigForDefaultDisplay())
		} else {
			mpm.createScreenCaptureIntent()
		}
		consent.launch(intent)
	}
}
