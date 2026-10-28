// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Test media for the native end-to-end test, made with the bundled ffmpeg
// from generated test patterns and a sine tone (never a real recording):
// an 8-second MP4 with picture and sound (Panopto's "podcast" stream and the
// generic page's file), and HLS in Panopto's shape — a single fragmented MP4
// addressed by byte ranges — for the lecture audio, the screen capture and a
// second screen capture with a different picture.

import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

function ff(ffmpeg, args) {
  execFileSync(ffmpeg, ['-hide_banner', '-loglevel', 'error', '-y', ...args], { stdio: ['ignore', 'ignore', 'pipe'], windowsHide: true });
}

function hls(ffmpeg, input, map, dir) {
  fs.mkdirSync(dir, { recursive: true });
  ff(ffmpeg, [
    '-i', input, '-map', map, '-c', 'copy',
    '-f', 'hls', '-hls_segment_type', 'fmp4', '-hls_flags', 'single_file', '-hls_time', '2', '-hls_playlist_type', 'vod',
    '-hls_segment_filename', path.join(dir, 'fragmented.mp4'), path.join(dir, 'index.m3u8'),
  ]);
}

/**
 * Writes the media into dir and returns their paths: full (MP4, picture and
 * sound), audio / screen / screen2 (directories with index.m3u8 and
 * fragmented.mp4).
 */
export function makeMedia(ffmpeg, dir) {
  fs.mkdirSync(dir, { recursive: true });
  const full = path.join(dir, 'full.mp4');
  const second = path.join(dir, 'second.mp4');
  ff(ffmpeg, [
    '-f', 'lavfi', '-i', 'testsrc2=size=320x180:rate=15', '-f', 'lavfi', '-i', 'sine=frequency=440:sample_rate=48000',
    '-t', '8', '-c:v', 'libopenh264', '-b:v', '250k', '-g', '30', '-c:a', 'aac', '-b:a', '64k', '-ac', '1', '-shortest', '-movflags', '+faststart', full,
  ]);
  ff(ffmpeg, ['-f', 'lavfi', '-i', 'smptebars=size=320x180:rate=15', '-t', '8', '-c:v', 'libopenh264', '-b:v', '150k', '-g', '30', '-an', second]);
  const out = { full, audio: path.join(dir, 'audio'), screen: path.join(dir, 'screen'), screen2: path.join(dir, 'screen2') };
  hls(ffmpeg, full, '0:a', out.audio);
  hls(ffmpeg, full, '0:v', out.screen);
  hls(ffmpeg, second, '0:v', out.screen2);
  return out;
}

/** The streams of a media file, as ffprobe sees them: ['video', 'audio', …]. */
export function streamKinds(ffprobe, file) {
  const out = execFileSync(ffprobe, ['-v', 'error', '-show_entries', 'stream=codec_type', '-of', 'csv=p=0', file], { encoding: 'utf8', windowsHide: true });
  return out.split(/\r?\n/).map((s) => s.trim()).filter(Boolean);
}
