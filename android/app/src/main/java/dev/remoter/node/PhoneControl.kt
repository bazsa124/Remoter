package dev.remoter.node

import android.accessibilityservice.AccessibilityService
import android.accessibilityservice.GestureDescription
import android.graphics.Bitmap
import android.graphics.Path
import android.graphics.Point
import android.os.Build
import android.os.Bundle
import android.view.Display
import android.view.WindowManager
import android.view.accessibility.AccessibilityEvent
import android.view.accessibility.AccessibilityNodeInfo
import androidx.annotation.RequiresApi
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference

/**
 * The phone's eyes and hands, through the Accessibility API.
 *
 * This is the unattended path: once the owner enables the service, the phone
 * can be seen and driven with nobody holding it - no "Start now" prompt, unlike
 * screen recording. The cost is the frame rate: Android rate-limits
 * accessibility screenshots, to a few per second at best.
 *
 * It listens to no accessibility events and collects nothing. It acts only when
 * the node service asks, which happens only for a session the hub relayed.
 */
class PhoneControl : AccessibilityService() {

	override fun onServiceConnected() {
		instance = this
	}

	override fun onUnbind(intent: android.content.Intent?): Boolean {
		if (instance === this) instance = null
		return super.onUnbind(intent)
	}

	override fun onDestroy() {
		if (instance === this) instance = null
		super.onDestroy()
	}

	override fun onAccessibilityEvent(event: AccessibilityEvent?) {}
	override fun onInterrupt() {}

	/** The physical screen size in pixels. */
	fun screenSize(): Point {
		val wm = getSystemService(WindowManager::class.java)
		return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
			val b = wm.currentWindowMetrics.bounds
			Point(b.width(), b.height())
		} else {
			@Suppress("DEPRECATION")
			Point().also { wm.defaultDisplay.getRealSize(it) }
		}
	}

	/**
	 * Takes one screenshot, blocking up to [timeoutMs]. Null when Android
	 * refuses - too soon after the last one, or a secure window (banking apps,
	 * the PIN pad) is showing.
	 */
	fun screenshot(timeoutMs: Long = 2_000): Bitmap? {
		if (Build.VERSION.SDK_INT < Build.VERSION_CODES.R) return null
		return screenshotR(timeoutMs)
	}

	@RequiresApi(Build.VERSION_CODES.R)
	private fun screenshotR(timeoutMs: Long): Bitmap? {
		val done = CountDownLatch(1)
		val out = AtomicReference<Bitmap?>(null)
		takeScreenshot(Display.DEFAULT_DISPLAY, mainExecutor, object : TakeScreenshotCallback {
			override fun onSuccess(result: ScreenshotResult) {
				val buffer = result.hardwareBuffer
				try {
					// A hardware bitmap cannot be read pixel by pixel; copy it into
					// ordinary memory before the buffer goes back to the system.
					val hw = Bitmap.wrapHardwareBuffer(buffer, result.colorSpace)
					out.set(hw?.copy(Bitmap.Config.ARGB_8888, false))
					hw?.recycle()
				} finally {
					buffer.close()
					done.countDown()
				}
			}

			override fun onFailure(errorCode: Int) {
				lastError = errorCode
				done.countDown()
			}
		})
		done.await(timeoutMs, TimeUnit.MILLISECONDS)
		return out.get()
	}

	/** A tap, or a press held for [holdMs] (long press = 600 ms and up). */
	fun tap(x: Float, y: Float, holdMs: Long = 60) {
		val path = Path().apply { moveTo(x, y) }
		gesture(GestureDescription.StrokeDescription(path, 0, holdMs.coerceAtLeast(1)))
	}

	/** A straight-line swipe; [ms] sets the speed (short = fling). */
	fun swipe(x1: Float, y1: Float, x2: Float, y2: Float, ms: Long) {
		val path = Path().apply {
			moveTo(x1, y1)
			lineTo(x2, y2)
		}
		gesture(GestureDescription.StrokeDescription(path, 0, ms.coerceIn(10, 3_000)))
	}

	private fun gesture(stroke: GestureDescription.StrokeDescription) {
		dispatchGesture(GestureDescription.Builder().addStroke(stroke).build(), null, null)
	}

	/**
	 * A drag along [points], after holding the first one still for [holdMs].
	 *
	 * The hold is what makes it a drag rather than a swipe: a launcher icon, a
	 * text selection handle or a list item only "picks up" after the finger has
	 * rested ~500 ms. One stroke cannot pause - a path's timing follows its
	 * length - so the hold is its own stroke, continued into the movement.
	 */
	fun drag(points: List<android.graphics.PointF>, holdMs: Long, ms: Long) {
		if (points.size < 2) return
		val start = points[0]
		val travel = Path().apply {
			moveTo(start.x, start.y)
			points.drop(1).forEach { lineTo(it.x, it.y) }
		}
		if (holdMs <= 0) {
			gesture(GestureDescription.StrokeDescription(travel, 0, ms.coerceIn(50, 5_000)))
			return
		}
		// A one-pixel nudge: a zero-length path has no duration to hold.
		val hold = GestureDescription.StrokeDescription(
			Path().apply {
				moveTo(start.x, start.y)
				lineTo(start.x + 1f, start.y)
			},
			0, holdMs.coerceIn(100, 3_000), true,
		)
		dispatchGesture(GestureDescription.Builder().addStroke(hold).build(), object : GestureResultCallback() {
			override fun onCompleted(gestureDescription: GestureDescription?) {
				val path = Path().apply {
					moveTo(start.x + 1f, start.y)
					points.drop(1).forEach { lineTo(it.x, it.y) }
				}
				val move = hold.continueStroke(path, 0, ms.coerceIn(50, 5_000), false)
				dispatchGesture(GestureDescription.Builder().addStroke(move).build(), null, null)
			}
		}, null)
	}

	/** Back, Home, Recents, notifications - the buttons a phone has instead of keys. */
	fun nav(action: String): Boolean {
		val global = when (action) {
			"back" -> GLOBAL_ACTION_BACK
			"home" -> GLOBAL_ACTION_HOME
			"recents" -> GLOBAL_ACTION_RECENTS
			"notifications" -> GLOBAL_ACTION_NOTIFICATIONS
			"quicksettings" -> GLOBAL_ACTION_QUICK_SETTINGS
			"power" -> GLOBAL_ACTION_POWER_DIALOG
			"lock" -> GLOBAL_ACTION_LOCK_SCREEN
			else -> return false
		}
		return performGlobalAction(global)
	}

	/**
	 * Types into whatever field has input focus, by rewriting its text.
	 *
	 * There is no keyboard to press here: an accessibility service can only set
	 * a field's whole text. So typing appends, and Backspace removes the last
	 * character. Good enough for messages and search boxes; not for a field that
	 * reacts to each key press.
	 */
	fun type(text: String) = editFocused { current -> current + text }

	fun backspace() = editFocused { current -> current.dropLast(1) }

	/** Enter: the field's "send"/"go" action where it has one. */
	fun enter() {
		val node = focusedInput() ?: return
		if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
			node.performAction(AccessibilityNodeInfo.AccessibilityAction.ACTION_IME_ENTER.id)
		}
	}

	private fun focusedInput(): AccessibilityNodeInfo? =
		rootInActiveWindow?.findFocus(AccessibilityNodeInfo.FOCUS_INPUT)

	private fun editFocused(change: (String) -> String) {
		val node = focusedInput() ?: return
		if (!node.isEditable) return
		// A field showing only its hint reports the hint as its text.
		val current = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O && node.isShowingHintText) ""
		else node.text?.toString().orEmpty()
		val args = Bundle().apply {
			putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, change(current))
		}
		node.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, args)
	}

	companion object {
		/** The running service, or null when the owner has not enabled it. */
		@Volatile
		var instance: PhoneControl? = null
			private set

		/** The last screenshot failure code, for diagnostics. */
		@Volatile
		var lastError: Int = 0
	}
}
