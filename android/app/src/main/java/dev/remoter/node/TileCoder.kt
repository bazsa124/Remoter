package dev.remoter.node

import android.graphics.Bitmap
import java.io.ByteArrayOutputStream
import java.io.DataOutputStream

/**
 * Live's tile differencing, ported from the Windows node (live.go) byte for
 * byte, so the same viewer renders a phone and a PC.
 *
 * A frame is cut into 128 px tiles; only tiles whose pixels changed are JPEG
 * encoded and sent. A phone screen that is not changing costs nothing.
 *
 * Wire format, big-endian:
 *   header 13 bytes: width, height, cols, rows, tileSize, tileCount (u16), keyframe (u8)
 *   per tile:        index (u16), length (u32), JPEG bytes
 *
 * The hashes are the model of what the viewer currently shows, so they are only
 * updated once a frame has actually been sent: [commit] after a successful
 * write, [discard] after a failed one.
 */
class TileCoder {

	private var hashes = LongArray(0)
	private var cols = 0
	private var rows = 0
	private val staged = ArrayList<Pair<Int, Long>>()
	private var pixels = IntArray(0)
	private val jpeg = ByteArrayOutputStream()
	private val body = ByteArrayOutputStream()

	/** The changed tiles of [frame] as one Live message, or null if nothing changed. */
	fun encode(frame: Bitmap, quality: Int, keyframe: Boolean): ByteArray? {
		val width = frame.width
		val height = frame.height
		val c = (width + TILE - 1) / TILE
		val r = (height + TILE - 1) / TILE
		var key = keyframe
		if (c != cols || r != rows) {
			cols = c
			rows = r
			hashes = LongArray(c * r)
			key = true
		}

		staged.clear()
		body.reset()
		val out = DataOutputStream(body)
		for (ty in 0 until r) {
			for (tx in 0 until c) {
				val x0 = tx * TILE
				val y0 = ty * TILE
				val w = minOf(TILE, width - x0)
				val h = minOf(TILE, height - y0)
				val need = w * h
				if (pixels.size < need) pixels = IntArray(need)
				frame.getPixels(pixels, 0, w, x0, y0, w, h)

				val sum = fnv(pixels, need)
				val idx = ty * c + tx
				if (!key && hashes[idx] == sum) continue

				val tile = Bitmap.createBitmap(frame, x0, y0, w, h)
				jpeg.reset()
				tile.compress(Bitmap.CompressFormat.JPEG, quality, jpeg)
				tile.recycle()

				out.writeShort(idx)
				out.writeInt(jpeg.size())
				jpeg.writeTo(out)
				staged.add(idx to sum)
			}
		}
		if (staged.isEmpty()) return null
		out.flush()

		val header = ByteArrayOutputStream(13 + body.size())
		DataOutputStream(header).apply {
			writeShort(width)
			writeShort(height)
			writeShort(c)
			writeShort(r)
			writeShort(TILE)
			writeShort(staged.size)
			writeByte(if (key) 1 else 0)
			flush()
		}
		body.writeTo(header)
		return header.toByteArray()
	}

	/** The viewer received the last frame: its tiles are now on screen there. */
	fun commit() {
		for ((idx, sum) in staged) if (idx < hashes.size) hashes[idx] = sum
		staged.clear()
	}

	/** The last frame never arrived: send its tiles again next time. */
	fun discard() = staged.clear()

	private fun fnv(px: IntArray, n: Int): Long {
		var h = -0x340d631b7bdddcdbL // FNV-1a 64 offset basis
		for (i in 0 until n) {
			h = (h xor (px[i].toLong() and 0xffffffffL)) * 0x100000001b3L
		}
		return h
	}

	companion object {
		const val TILE = 128
	}
}
