package dev.remoter.node

import android.content.Context
import android.graphics.Bitmap
import android.graphics.PixelFormat
import android.hardware.display.DisplayManager
import android.hardware.display.VirtualDisplay
import android.media.ImageReader
import android.media.projection.MediaProjection
import android.os.Build
import android.os.Handler
import android.os.HandlerThread
import android.view.WindowManager
import kotlin.math.roundToInt

/** Where Live's frames come from. */
interface FrameSource {
	/** The fastest this source can usefully be asked. */
	val maxFps: Int

	/** A frame at roughly [scale] of the screen, or null when there is none yet. */
	fun grab(scale: Double): Bitmap?

	/** True once the source can no longer deliver (projection revoked). */
	val ended: Boolean

	fun close()
}

/**
 * Unattended frames: accessibility screenshots. Works with nobody at the phone,
 * but Android allows only a few a second.
 */
class AccessibilitySource(private val control: PhoneControl) : FrameSource {
	override val maxFps = 3
	override val ended = false

	override fun grab(scale: Double): Bitmap? {
		val shot = control.screenshot() ?: return null
		val scaled = NodeServer.scaleOf(shot, scale)
		if (scaled !== shot) shot.recycle()
		return scaled
	}

	override fun close() {}
}

/**
 * Smooth frames: a MediaProjection mirroring the screen into an ImageReader.
 *
 * Needs someone at the phone to tap "Start now" (Android 14+ asks every time),
 * which is why it is an upgrade and not the default. The capture is made at the
 * target size directly, so there is no scaling pass, and the frame rate is
 * bounded by encoding rather than by a rate limit.
 *
 * Android 14 allows one virtual display per projection, so scale changes and
 * rotations resize the existing one instead of creating another.
 */
class ProjectionSource(
	private val context: Context,
	private val projection: MediaProjection,
) : FrameSource {

	override val maxFps = 10

	@Volatile
	override var ended = false
		private set

	private val thread = HandlerThread("projection").apply { start() }
	private val handler = Handler(thread.looper)
	private var display: VirtualDisplay? = null
	private var reader: ImageReader? = null
	private var width = 0
	private var height = 0

	init {
		// Required before createVirtualDisplay on Android 14, and how we learn
		// that the owner stopped the cast from the status bar.
		projection.registerCallback(object : MediaProjection.Callback() {
			override fun onStop() {
				ended = true
			}
		}, handler)
	}

	override fun grab(scale: Double): Bitmap? {
		if (ended) return null
		ensure(scale)
		val image = reader?.acquireLatestImage() ?: return null
		return image.use { img ->
			val plane = img.planes[0]
			val pixelStride = plane.pixelStride
			val padded = plane.rowStride / pixelStride
			val full = Bitmap.createBitmap(padded, img.height, Bitmap.Config.ARGB_8888)
			full.copyPixelsFromBuffer(plane.buffer)
			if (padded == img.width) full
			else Bitmap.createBitmap(full, 0, 0, img.width, img.height).also { full.recycle() }
		}
	}

	private fun ensure(scale: Double) {
		val wm = context.getSystemService(WindowManager::class.java)
		val (sw, sh, dpi) = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
			val m = wm.currentWindowMetrics
			Triple(m.bounds.width(), m.bounds.height(), context.resources.displayMetrics.densityDpi)
		} else {
			val dm = context.resources.displayMetrics
			Triple(dm.widthPixels, dm.heightPixels, dm.densityDpi)
		}
		// Even sizes: some encoders behind VirtualDisplay dislike odd ones.
		val w = ((sw * scale).roundToInt() / 2 * 2).coerceAtLeast(16)
		val h = ((sh * scale).roundToInt() / 2 * 2).coerceAtLeast(16)
		if (w == width && h == height && display != null) return

		val old = reader
		val next = ImageReader.newInstance(w, h, PixelFormat.RGBA_8888, 2)
		val vd = display
		if (vd == null) {
			display = projection.createVirtualDisplay(
				"remoter-live", w, h, dpi,
				DisplayManager.VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR,
				next.surface, null, handler,
			)
		} else {
			vd.resize(w, h, dpi)
			vd.surface = next.surface
		}
		reader = next
		old?.close()
		width = w
		height = h
	}

	override fun close() {
		ended = true
		display?.release()
		display = null
		reader?.close()
		reader = null
		runCatching { projection.stop() }
		thread.quitSafely()
	}
}
