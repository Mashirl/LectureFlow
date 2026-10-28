// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Installing in-app updates at the next start (SPEC-ext "应用内更新" and
// M5; the contract with the Go side is internal/selfupdate/README.md "The
// updates directory"). Go verifies a package and leaves pending.json in
// {data_dir}/updates; this module, run by the shell before it starts the
// service, installs it, keeps the previous version and goes back to it when
// the new version does not start.
//
//   start (old version)  pending.json valid and vouched for by the installed
//                        service program (-selfupdate-verify, its built-in
//                        public key: lib/verifyupdate.js), which also copies
//                        the package into a private temporary folder: only
//                        that copy is run (security review sec-update M3) →
//                        back up what previous.json lacks, snapshot the
//                        database, record the installation in the trust
//                        file, rename pending.json to applying.json,
//                        install, quit:
//                          installer  NSIS --updated /S --force-run /D=<dir>
//                                     (it closes this app, installs per user
//                                     and starts the new version)
//                          portable   replace PORTABLE_EXECUTABLE_FILE, start it
//                          zip / dmg  unpack the .app next to the current one,
//                                     check it, swap, keep the old one in
//                                     backup/, start the new one
//   start (new version)  applying.json names this version → probation: the
//                        shell starts the service; once it is healthy and the
//                        window has loaded, result.json "installed". A failed
//                        start is retried once ("two starts in a row"); then
//                        the database snapshot and the previous version are
//                        put back (applying.json "rolling_back") and the
//                        previous version is started.
//   start (old again)    applying.json "rolling_back" → result.json
//                        "rolled_back" with the reason; Go shows it.
//
// What a rollback runs is checked like an update (sec-update M2): the
// previous version's package by the service program (-selfupdate-verify
// -selfupdate-previous: signature, size, SHA-256, older version, run from
// a private copy); a backup of the installation by the hash of its whole
// tree recorded when this shell made it. The service program also says
// whether other accounts can change the data folder (exit 3): then nothing
// in it is installed or rolled back.
//
// The trust file (F.TRUST_FILE in the shell's profile, userData; never in
// a moved data folder) is this module's own record:
//   install   { version, from_version, target, kind, started_at }: the
//             installation this shell started. applying.json counts only
//             when it matches (a planted applying.json is ignored), and its
//             target must be this installation's place.
//   backups   { <version>: { dir, kind, item, tree, files, bytes, created_at } }:
//             the backups this shell made (tree: F.treeHash of backup/<v>/app/<item>).
// Why a hash rather than a code signature: builds may be unsigned, and a
// hash pins this very copy (a signature would let any older signed version
// pass); the profile is the user's own.
//
// applying.json is this module's own file (Go never reads it): pending.json's
// fields plus
//   phase       installing | rolling_back
//   tries       starts of the new version so far (at most MAX_TRIES)
//   target      what is replaced: installation directory, portable .exe or .app
//   exe         the executable's file name
//   started_at  when the installation began
//   db_snapshot backup/<from>/db (relative), the database before the update
//   pid, pid_at the installer or restore program started for it, and when
//   error       why the new version is being rolled back (or the last failed start)
//   restore     file | backup: what the rollback restores
//   restore_error  why the rollback failed (written by the restore program)
//
// Nothing here is installed while the service runs: the shell calls atStart
// before it starts the service, and stops the service before failed(), so no
// task can be running (SPEC-v3 V8). No Electron imports: effects that need
// Electron (single-instance lock, the profile folder) or start programs come
// in as ops.
'use strict';

const childProcess = require('node:child_process');
const os = require('node:os');
const path = require('node:path');

const F = require('./lib/updatefiles');
const { interpretVerify, COPY_DIR_RE } = require('./lib/verifyupdate');

const { fsp } = F;

const MAX_TRIES = 2;
// An installer or restore program started this recently may still be working.
const HELPER_WAIT_MS = 10 * 60 * 1000;
const RESTORE_ARG = '--lf-restore';
const UPDATED_ARG = '--updated';
const CONTINUE = Object.freeze({ action: 'continue' });

function stamp(d) {
  return d.toISOString().replace(/[-:]/g, '').replace(/\.\d+Z$/, 'Z');
}

function samePath(a, b) {
  if (!a || !b || typeof a !== 'string' || typeof b !== 'string') return false;
  const na = path.resolve(a);
  const nb = path.resolve(b);
  return process.platform === 'win32' ? na.toLowerCase() === nb.toLowerCase() : na === nb;
}

function validApplying(v) {
  return !!v && v.schema === F.SCHEMA && !!F.parseVersion(v.version) && !!F.parseVersion(v.from_version)
    && (v.phase === undefined || v.phase === 'installing' || v.phase === 'rolling_back');
}

class SelfUpdateApplier {
  /**
   * install: lib/updatefiles.describeInstallation() — null in development
   *   (nothing is touched), { unsupported } for a packaged copy that cannot be
   *   updated in place, else { platform, kind, kinds, target, exe }.
   * currentVersion: the running version (app.getVersion()).
   * dataDir: the service's data directory; updates are in {dataDir}/updates.
   * privateDir: where the trust file lives (default ops.privateDir(): the
   *   shell's profile); tmpDir: where the service program puts the private
   *   copies (default ops.tmpDir(), else the system's temporary folder).
   * ops: {
   *   spawnDetached(file, args, { verbatim }) → Promise<pid>  a program that outlives this app
   *   run(file, args) → Promise<{ stdout }>                   helper programs (macOS: ditto, codesign, …)
   *   verifyPending(updatesDir, version, { previous, copyTo }) → Promise<{ code, stdout, stderr, timedOut, error }>
   *                                                           the installed service program's
   *                                                           -selfupdate-verify (lib/verifyupdate.js)
   *   releaseLock()                                           let the next instance take the single-instance lock
   *   pidAlive(pid) → boolean
   *   privateDir() → string, tmpDir() → string               (optional, see above)
   * }
   */
  constructor({ install, currentVersion, dataDir, ops, log, now = () => new Date(), privateDir = null, tmpDir = null }) {
    this.install = install;
    this.version = currentVersion;
    this.dataDir = dataDir;
    this.dir = path.join(dataDir, 'updates');
    this.ops = ops;
    this.log = log || { info() {}, warn() {}, error() {} };
    this.now = now;
    this.privateDir = privateDir || (ops && typeof ops.privateDir === 'function' ? ops.privateDir() : null);
    this.tmpDir = path.resolve(tmpDir || (ops && typeof ops.tmpDir === 'function' ? ops.tmpDir() : null) || os.tmpdir());
    this.applying = null; // the applying.json of a probation in progress
  }

  file(name) {
    return path.join(this.dir, F.FILES[name]);
  }

  backupDir(version) {
    return path.join(this.dir, F.BACKUP_DIR, version);
  }

  // ---- at start ----

  /**
   * atStart({ background }) → what the shell does next:
   *   { action: 'continue' }   start the service as usual
   *   { action: 'probation' }  start it; report healthy() or failed(reason)
   *   { action: 'exit' }       an installer / the other version was started: quit
   * background: started by the browser extension's host without a window;
   * a pending update then waits for a normal start.
   */
  async atStart({ background = false } = {}) {
    if (!this.install) return CONTINUE;
    try {
      await this._cleanupLeftovers();
      await this._cleanupCopies();
      const read = await F.readJson(this.file('applying'));
      if (read.exists) {
        const r = await this._resolveApplying(read);
        if (r) return r;
      }
      return await this._handlePending({ background });
    } catch (err) {
      // Never keep the app from starting.
      this.log.error(`selfupdate: ${F.shortError(err)}`);
      return CONTINUE;
    }
  }

  async _resolveApplying(read) {
    if (read.error || !validApplying(read.value)) {
      this.log.warn(`selfupdate: ignoring applying.json (${read.error || 'not a valid applying.json'})`);
      await F.removeFile(this.file('applying'));
      return null;
    }
    const a = read.value;
    const v = this.version;
    if (this.install.unsupported) {
      await this._finish(a, 'failed', this.install.unsupported);
      return null;
    }
    const foreign = await this._foreignApplying(a);
    if (foreign) {
      // Not an installation this shell started (or not of this place):
      // nothing in it is run.
      this.log.warn(`selfupdate: ignoring applying.json (${a.from_version} → ${a.version}): ${foreign}`);
      await F.removeFile(this.file('applying'));
      return null;
    }
    if (a.phase === 'rolling_back') {
      if (F.sameVersion(v, a.from_version)) {
        await this._finish(a, 'rolled_back', a.error || `LectureFlow ${a.version} did not start`);
        return null;
      }
      if (F.sameVersion(v, a.version)) {
        if (this._helperRunning(a)) return this._waitForHelper(a);
        const why = a.restore_error ? `: ${a.restore_error}` : '';
        await this._finish(a, 'failed', `${a.error || `LectureFlow ${a.version} did not start`}; going back to ${a.from_version} failed${why}`);
        return null;
      }
    } else {
      if (F.sameVersion(v, a.version)) return this._beginProbation(a);
      if (F.sameVersion(v, a.from_version)) {
        if (this._helperRunning(a)) return this._waitForHelper(a);
        await this._finish(a, 'failed', a.error || `the installer did not install ${a.version}; ${v} is still installed`);
        return null;
      }
    }
    // Neither version runs: something else was installed by hand.
    this.log.warn(`selfupdate: ${v} runs, applying.json is about ${a.from_version} → ${a.version}; dropping it`);
    await F.removeFile(this.file('applying'));
    return null;
  }

  /**
   * _foreignApplying(a) → why applying.json is not this shell's own
   * installation of this place (null when it is): the trust file must
   * record the same versions and target, and the target must be where this
   * copy of LectureFlow is installed.
   */
  async _foreignApplying(a) {
    if (!samePath(a.target, this.install.target)) return 'its target is not where LectureFlow is installed';
    const rec = (await this._readTrust()).install;
    if (!rec) return 'this LectureFlow did not start an installation (no record in its profile)';
    if (!F.sameVersion(rec.version, a.version) || !F.sameVersion(rec.from_version, a.from_version) || !samePath(rec.target, a.target)) {
      return `the installation this LectureFlow started was ${rec.from_version} → ${rec.version}`;
    }
    return null;
  }

  _helperRunning(a) {
    if (!Number.isInteger(a.pid) || a.pid <= 0) return false;
    const at = Date.parse(a.pid_at || '');
    if (!Number.isFinite(at) || this.now().getTime() - at > HELPER_WAIT_MS) return false;
    return !!this.ops.pidAlive(a.pid);
  }

  _waitForHelper(a) {
    // The installer (or the restore program) will start LectureFlow itself.
    this.log.info(`selfupdate: the update to ${a.version} is still being ${a.phase === 'rolling_back' ? 'rolled back' : 'installed'} (pid ${a.pid}); this start ends`);
    return { action: 'exit', why: 'busy' };
  }

  async _beginProbation(a) {
    const tries = (Number.isInteger(a.tries) && a.tries > 0 ? a.tries : 0) + 1;
    if (tries > MAX_TRIES) {
      const r = await this._rollback(a, a.error || `LectureFlow ${a.version} did not start ${MAX_TRIES} times in a row`);
      return r.action === 'exit' ? r : CONTINUE;
    }
    a.tries = tries;
    delete a.pid;
    delete a.pid_at;
    await F.writeJsonAtomic(this.file('applying'), a);
    // Written now so that the service (which reads result.json at its start)
    // reports it; a rollback replaces it.
    await this._writeResult(a, 'installed');
    this.applying = a;
    this.log.info(`selfupdate: LectureFlow ${a.version} starts for the first time after the update (try ${tries} of ${MAX_TRIES})`);
    return { action: 'probation', version: a.version, tries };
  }

  // ---- probation (new version) ----

  /** inProbation: the shell is starting a freshly installed version. */
  get inProbation() {
    return !!this.applying;
  }

  /** healthy: the service is up and the window has loaded the app. */
  async healthy() {
    const a = this.applying;
    if (!a) return;
    this.applying = null;
    try {
      await this._finish(a, 'installed');
      this.log.info(`selfupdate: LectureFlow ${a.version} is installed`);
    } catch (err) {
      this.log.warn(`selfupdate: finishing the update: ${F.shortError(err)}`);
    }
  }

  /**
   * failed(reason): the new version failed to start (the shell has stopped
   * the service). → { action: 'retry' } start once more,
   * { action: 'exit' } the previous version is being put back: quit,
   * { action: 'giveup', error } nothing to go back to: show the error.
   */
  async failed(reason) {
    const a = this.applying;
    if (!a) return { action: 'none' };
    this.applying = null;
    a.error = F.shortError(reason) || 'the new version did not start';
    try {
      if (a.tries < MAX_TRIES) {
        await F.writeJsonAtomic(this.file('applying'), a);
        this.log.warn(`selfupdate: LectureFlow ${a.version} did not start (${a.error}); trying once more`);
        return { action: 'retry' };
      }
      return await this._rollback(a, a.error);
    } catch (err) {
      this.log.error(`selfupdate: ${F.shortError(err)}`);
      return { action: 'giveup', error: F.shortError(err) };
    }
  }

  // ---- rollback ----

  async _rollback(a, error) {
    error = F.shortError(error);
    this.log.warn(`selfupdate: going back to ${a.from_version}: ${error}`);
    const prev = await this._readPrevious();
    const src = await this._restoreSource(prev, a);
    if (!src.path) {
      await this._finish(a, 'failed', `${error}; ${src.error}`);
      return { action: 'giveup', error: `${error}; ${src.error}` };
    }
    try {
      await this._restoreDb(a);
    } catch (err) {
      this.log.warn(`selfupdate: restoring the database snapshot: ${F.shortError(err)}`);
    }
    a.phase = 'rolling_back';
    a.error = error;
    a.restore = src.kind;
    delete a.pid;
    delete a.pid_at;
    await F.writeJsonAtomic(this.file('applying'), a);
    try {
      const pid = await this._restore(src, a, prev);
      if (pid) {
        a.pid = pid;
        a.pid_at = this.now().toISOString();
        await F.writeJsonAtomic(this.file('applying'), a);
      }
      return { action: 'exit' };
    } catch (err) {
      await this._removeCopy(src.copy);
      const msg = `${error}; going back to ${a.from_version} failed: ${F.shortError(err)}`;
      await this._finish(a, 'failed', msg);
      return { action: 'giveup', error: msg };
    }
  }

  /**
   * _restoreSource → { kind: 'file', path, packageKind, copy } (the
   * previous version's package, verified by the service program, as its
   * private copy) or { kind: 'backup', path } (a backup whose tree hash
   * matches the trust file), or { error }.
   */
  async _restoreSource(prev, a) {
    if (!prev) return { error: 'previous.json is missing' };
    if (!F.sameVersion(prev.version, a.from_version)) return { error: `previous.json describes ${prev.version}, not ${a.from_version}` };
    const checked = await this._verifyPrevious(prev);
    if (checked.error) return { error: checked.error };
    const file = checked.file;
    const backup = await this._backupItem(prev);
    const order = this.install.kind === F.KINDS.zip ? [backup, file] : [file, backup];
    const pick = order.find((x) => x);
    if (file && pick !== file) await this._removeCopy(file.copy);
    return pick || { error: `no package or backup of ${prev.version} to return to${checked.why ? ` (its package: ${checked.why})` : ''}` };
  }

  /**
   * _verifyPrevious(prev) → { file } (null: no usable package; the data
   * folder is trusted, so a backup may be used) or { error } (no rollback
   * at all: other accounts can change the data folder, or the service
   * program gave no verdict, so that cannot be ruled out).
   */
  async _verifyPrevious(prev) {
    if (!this.ops || typeof this.ops.verifyPending !== 'function') {
      return { error: 'this copy of LectureFlow has no update check program to vouch for the previous version' };
    }
    let run;
    try {
      run = await this.ops.verifyPending(this.dir, this.version, { previous: true, copyTo: this.tmpDir });
    } catch (err) {
      run = { error: F.shortError(err) };
    }
    const expected = { version: prev.version, kind: prev.kind, file: prev.file, sha256: prev.sha256 };
    const v = interpretVerify(run, expected, { copyTo: this.tmpDir, what: 'previous' });
    const accepted = this.install.kind === F.KINDS.zip ? [F.KINDS.zip, F.KINDS.dmg] : [this.install.kind];
    switch (v.verdict) {
      case 'ok':
        if (!prev.file || !accepted.includes(prev.kind)) {
          await this._removeCopy(v.copy);
          return { file: null };
        }
        this.log.info(`selfupdate: the service program verified the package of ${prev.version} with its built-in key`);
        return { file: { kind: 'file', path: v.copy, packageKind: prev.kind, copy: v.copy } };
      case 'invalid':
      case 'not_configured':
        // (a build without a public key never runs a package it cannot verify)
        await this._removeCopy(v.copy);
        if (!prev.file) return { file: null };
        this.log.warn(`selfupdate: the package of ${prev.version} cannot be used: ${v.drop || v.keep}`);
        return { file: null, why: v.drop || v.keep };
      case 'untrusted':
        return { error: v.keep };
      default:
        return { error: `the previous version could not be checked: ${v.keep}` };
    }
  }

  /**
   * _previousFileUsable(prev): previous.json names a package of this kind
   * that is still there with its size and SHA-256 (then no backup of the
   * installation is made; the package itself is verified by the service
   * program before a rollback runs it).
   */
  async _previousFileUsable(prev) {
    if (!prev.file) return false;
    const file = F.resolveInside(this.dir, prev.file);
    if (!file) return false;
    const accepted = this.install.kind === F.KINDS.zip ? [F.KINDS.zip, F.KINDS.dmg] : [this.install.kind];
    if (!accepted.includes(prev.kind)) return false;
    const bad = await F.checkFile(file, prev.size, prev.sha256);
    if (bad) {
      this.log.warn(`selfupdate: the package of ${prev.version} cannot be used: ${bad}`);
      return false;
    }
    return true;
  }

  /**
   * _backupItem(prev, { hash }) → { kind: 'backup', path } for a complete
   * backup of prev.version that this shell made: backup.json, the trust
   * file's record (same folder, kind and item) and, with hash (the
   * default), the tree hash of what it holds now.
   */
  async _backupItem(prev, { hash = true } = {}) {
    if (!prev.backup) return null;
    const dir = F.resolveInside(this.dir, prev.backup);
    if (!dir) return null;
    const mark = await F.readJson(path.join(dir, F.BACKUP_MARK));
    const m = mark.value;
    if (!m || m.schema !== F.SCHEMA || !F.sameVersion(m.version, prev.version) || m.kind !== this.install.kind || typeof m.item !== 'string') return null;
    if (m.item.includes('/') || m.item.includes('\\') || m.item === '..') return null;
    const item = path.join(dir, 'app', m.item === '.' ? '' : m.item);
    const probe = this.install.kind === F.KINDS.installer ? path.join(item, this.install.exe) : item;
    if (!(await F.exists(probe))) return null;
    const rec = ((await this._readTrust()).backups || {})[prev.version];
    if (!rec || rec.dir !== F.relativeTo(this.dir, dir) || rec.kind !== m.kind || rec.item !== m.item || typeof rec.tree !== 'string') {
      this.log.warn(`selfupdate: the backup of ${prev.version} was not made by this LectureFlow (no record in its profile); it is not used`);
      return null;
    }
    if (hash) {
      let h;
      try {
        h = await F.treeHash(item);
      } catch (err) {
        this.log.warn(`selfupdate: reading the backup of ${prev.version}: ${F.shortError(err)}`);
        return null;
      }
      if (h.sha256 !== rec.tree) {
        this.log.warn(`selfupdate: the backup of ${prev.version} changed since it was made; it is not used`);
        return null;
      }
    }
    return { kind: 'backup', path: item };
  }

  async _restore(src, a, prev) {
    const { kind, target } = this.install;
    if (kind === F.KINDS.installer) {
      if (src.kind === 'file') return this.ops.spawnDetached(src.path, F.nsisArguments(target), { verbatim: true });
      // The copy of the previous installation puts itself back (restoreFromBackup).
      this.ops.releaseLock();
      return this.ops.spawnDetached(path.join(src.path, this.install.exe), [RESTORE_ARG]);
    }
    if (kind === F.KINDS.portable) {
      await this._replaceFile(target, src.path);
      await this._removeCopy(src.copy);
      this.ops.releaseLock();
      return this._spawnQuiet(target, [UPDATED_ARG]);
    }
    // macOS
    if (src.kind === 'backup') await this._swapApp(src.path, null);
    else {
      try {
        await this._installMacPackage(src.path, src.packageKind, prev.version, null);
      } finally {
        await this._removeCopy(src.copy);
      }
    }
    this.ops.releaseLock();
    return this._spawnQuiet('/usr/bin/open', ['-n', target, '--args', UPDATED_ARG]);
  }

  async _restoreDb(a) {
    if (!a.db_snapshot) return;
    const dir = F.resolveInside(this.dir, a.db_snapshot);
    if (!dir || !(await F.isDir(dir))) return;
    for (const name of F.DB_FILES) {
      const snap = path.join(dir, name);
      const live = path.join(this.dataDir, name);
      if (await F.exists(snap)) {
        const tmp = `${live}.lf-restore`;
        await fsp.copyFile(snap, tmp);
        await F.renameRetry(tmp, live);
      } else {
        await fsp.rm(live, { force: true });
      }
    }
    this.log.info('selfupdate: database snapshot restored');
  }

  /**
   * abandonRestore(reason): the restore program could not start its work
   * (the failed version did not quit); the next start reports it.
   */
  async abandonRestore(reason) {
    const read = await F.readJson(this.file('applying'));
    const a = read.value;
    if (!a || !validApplying(a) || a.phase !== 'rolling_back') return;
    a.restore_error = F.shortError(reason);
    delete a.pid;
    delete a.pid_at;
    await F.writeJsonAtomic(this.file('applying'), a);
  }

  /**
   * restoreFromBackup: run by the backed-up copy of the previous Windows
   * installation (started with --lf-restore while the failed version quits):
   * it moves the failed installation aside, copies itself into its place
   * and starts it. Only for the installation and the backup recorded in
   * the trust file (the failed version checked the backup's tree hash
   * before it started this program). Returns true when it did so.
   */
  async restoreFromBackup({ execPath }) {
    const read = await F.readJson(this.file('applying'));
    const a = read.value;
    if (!a || !validApplying(a) || a.phase !== 'rolling_back' || a.restore !== 'backup') {
      this.log.warn('selfupdate: --lf-restore without a rollback in applying.json');
      return false;
    }
    const prev = await this._readPrevious();
    const backupDir = prev && prev.backup ? F.resolveInside(this.dir, prev.backup) : null;
    const myDir = path.dirname(execPath);
    const target = a.target;
    if (!backupDir || !samePath(myDir, path.join(backupDir, 'app')) || typeof target !== 'string' || !path.isAbsolute(target) || samePath(target, myDir)) {
      this.log.warn('selfupdate: --lf-restore from a copy that is not the recorded backup');
      return false;
    }
    const trust = await this._readTrust();
    const rec = trust.install;
    const brec = (trust.backups || {})[a.from_version];
    if (!rec || !F.sameVersion(rec.version, a.version) || !F.sameVersion(rec.from_version, a.from_version) || !samePath(rec.target, target)
      || !brec || brec.dir !== F.relativeTo(this.dir, backupDir)) {
      this.log.warn('selfupdate: --lf-restore for an installation or backup this LectureFlow did not record; nothing is replaced');
      return false;
    }
    const exe = path.basename(execPath);
    try {
      await this._replaceDir(target, myDir);
      this.log.info(`selfupdate: LectureFlow ${a.from_version} put back into ${target}`);
    } catch (err) {
      a.restore_error = F.shortError(err);
      this.log.error(`selfupdate: putting back ${a.from_version}: ${a.restore_error}`);
    }
    // The version started next must not wait for this program.
    delete a.pid;
    delete a.pid_at;
    await F.writeJsonAtomic(this.file('applying'), a).catch(() => {});
    this.ops.releaseLock();
    await this._spawnQuiet(path.join(target, exe), [UPDATED_ARG]);
    return true;
  }

  // ---- pending.json (old version) ----

  async _handlePending({ background }) {
    const read = await F.readJson(this.file('pending'));
    if (!read.exists) return CONTINUE;
    const check = await this._checkPending(read);
    if (check.drop) {
      this.log.info(`selfupdate: removing pending.json: ${check.drop}`);
      await F.removeFile(this.file('pending'));
      if (check.report) await this._writeResult({ version: check.version, from_version: this.version }, 'failed', check.drop);
      return CONTINUE;
    }
    if (background) {
      this.log.info(`selfupdate: LectureFlow ${check.pending.version} waits for a start with a window`);
      return CONTINUE;
    }
    const verdict = await this._verifyWithService(check.pending);
    if (!verdict.install) {
      await this._removeCopy(verdict.copy);
      const p = check.pending;
      if (verdict.drop) {
        this.log.warn(`selfupdate: not installing ${p.version}: ${verdict.drop}; removing pending.json`);
        await F.removeFile(this.file('pending'));
        await this._writeResult({ version: p.version, from_version: this.version }, 'failed', verdict.drop);
      } else {
        // No verdict: nothing is installed, pending.json stays (see
        // _verifyWithService); the result says why this start did not install it.
        this.log.warn(`selfupdate: not installing ${p.version} now: ${verdict.keep}; pending.json is kept`);
        await this._writeResult({ version: p.version, from_version: this.version }, 'failed', `${verdict.keep}; not installed at this start`);
      }
      return CONTINUE;
    }
    this.log.info(`selfupdate: the service program verified ${check.pending.version} with its built-in key`);
    return this._install(check.pending, verdict.copy);
  }

  /**
   * _verifyWithService(p): the installed service program checks the pending
   * update again with the public key built into it (lib/verifyupdate.js,
   * `-selfupdate-verify`) and copies the package into a private folder,
   * right before anything is backed up or started.
   * → { install: true, copy } | { drop: reason } | { keep: reason }.
   *
   * Only exit 0 naming exactly p, with its copy where it was asked for,
   * installs. Exit 1 ("invalid"): pending.json is removed like any other bad
   * update (the package directory is left to the service, which removes
   * unused version directories at its start). Exit 2 ("not_configured", a
   * build without a public key), exit 3 (other accounts can change the
   * data folder), a timeout, a program that cannot be run or an answer
   * outside the contract: not installed (nothing can vouch for the
   * package), but pending.json is kept. Such a build never writes
   * pending.json itself (Go writes it only after verifying with its key;
   * development builds have no installation and never get here), so a
   * pending update it finds belongs to another build sharing the data
   * directory, which can still verify and install it; once the data
   * folder's permissions are tightened, the update is installed; a timeout
   * or a program that does not start is tried again next time.
   */
  async _verifyWithService(p) {
    if (!this.ops || typeof this.ops.verifyPending !== 'function') {
      return { keep: 'this copy of LectureFlow has no update check program' };
    }
    let run;
    try {
      run = await this.ops.verifyPending(this.dir, this.version, { copyTo: this.tmpDir });
    } catch (err) {
      run = { error: F.shortError(err) };
    }
    return interpretVerify(run, p, { copyTo: this.tmpDir });
  }

  /** _checkPending → { pending, file } or { drop: reason, report, version }. */
  async _checkPending(read) {
    const p = read.value;
    const version = p && F.parseVersion(p.version) ? p.version : null;
    const drop = (why, report = true) => ({ drop: why, report: report && !!version, version });
    if (read.error || !p) return drop(read.error || 'pending.json is unreadable');
    if (p.schema !== F.SCHEMA) return drop(`pending.json has schema ${JSON.stringify(p.schema)}, not ${F.SCHEMA}`);
    if (!version) return drop('pending.json has no valid version');
    if (F.compareVersions(version, this.version) <= 0) return drop(`${version} is not newer than ${this.version}`, false);
    if (this.install.unsupported) return drop(this.install.unsupported);
    if (p.platform !== this.install.platform) return drop(`the package is for ${p.platform}, not ${this.install.platform}`);
    if (!this.install.kinds.includes(p.kind)) return drop(`this installation needs a "${this.install.kind}" package, not "${p.kind}"`);
    if (!Number.isSafeInteger(p.size) || p.size <= 0 || typeof p.sha256 !== 'string' || !/^[0-9a-f]{64}$/.test(p.sha256)) {
      return drop('pending.json has no valid size or SHA-256');
    }
    const file = F.resolveInside(this.dir, p.file);
    if (!file) return drop('pending.json names a file outside the updates folder');
    const bad = await F.checkFile(file, p.size, p.sha256);
    if (bad) return drop(bad);
    return { pending: p, file };
  }

  /** _install(p, copy): copy is the private copy of the package the service program made. */
  async _install(p, copy) {
    const v = this.version;
    const { kind, target } = this.install;
    const failEarly = async (why) => {
      await this._removeCopy(copy);
      await F.removeFile(this.file('pending'));
      await this._writeResult({ version: p.version, from_version: v }, 'failed', why);
      this.log.warn(`selfupdate: not installing ${p.version}: ${why}`);
      return CONTINUE;
    };
    if (!copy) return failEarly('the update check made no private copy of the package');
    if (!this.privateDir) return failEarly('this copy of LectureFlow has no profile folder to record the installation in');
    const prev = await this._readPrevious();
    if (!prev || !F.sameVersion(prev.version, v) || prev.platform !== this.install.platform) {
      return failEarly('previous.json does not describe the running version');
    }
    this.log.info(`selfupdate: installing LectureFlow ${p.version} (${kind}) over ${v}`);

    // 1. What a failed update returns to, and the database before it.
    let dbRel = null;
    try {
      if ((kind === F.KINDS.installer || kind === F.KINDS.portable) && !(await this._previousFileUsable(prev)) && !(await this._backupItem(prev))) {
        prev.backup = await this._backupInstallation();
        await F.writeJsonAtomic(this.file('previous'), prev);
      }
      dbRel = await this._snapshotDb();
    } catch (err) {
      return failEarly(`could not back up LectureFlow ${v}: ${F.shortError(err)}`);
    }

    // 2. The trust file, then pending.json → applying.json (a crash from
    // here on is noticed).
    const startedAt = this.now().toISOString();
    try {
      await this._updateTrust((t) => {
        t.install = { version: p.version, from_version: v, target, kind, started_at: startedAt };
      });
    } catch (err) {
      return failEarly(`could not record the installation in ${this.privateDir}: ${F.shortError(err)}`);
    }
    await F.renameRetry(this.file('pending'), this.file('applying'));
    const a = {
      ...p,
      phase: 'installing',
      tries: 0,
      target,
      exe: this.install.exe,
      started_at: startedAt,
    };
    if (dbRel) a.db_snapshot = dbRel;
    await F.writeJsonAtomic(this.file('applying'), a);

    // 3. Install (from the private copy).
    try {
      const pid = await this._installPackage(a, copy, prev);
      if (pid) {
        a.pid = pid;
        a.pid_at = this.now().toISOString();
        await F.writeJsonAtomic(this.file('applying'), a);
      }
      return { action: 'exit', version: a.version };
    } catch (err) {
      await this._removeCopy(copy);
      const why = `could not install ${a.version}: ${F.shortError(err)}`;
      this.log.warn(`selfupdate: ${why}`);
      await this._finish(a, 'failed', why);
      return CONTINUE;
    }
  }

  async _installPackage(a, file, prev) {
    const { kind, target } = this.install;
    if (kind === F.KINDS.installer) {
      // The installer closes this app (it is in the directory being
      // replaced), installs for this user only and starts the new version.
      // It runs from the private copy, which a later start removes.
      return this.ops.spawnDetached(file, F.nsisArguments(target), { verbatim: true });
    }
    if (kind === F.KINDS.portable) {
      await this._replaceFile(target, file);
      await this._removeCopy(file);
      this.ops.releaseLock();
      return this._spawnQuiet(target, [UPDATED_ARG]);
    }
    // macOS: the old .app becomes the backup of the previous version.
    const backupDir = this.backupDir(this.version);
    const keepOld = path.join(backupDir, 'app', path.basename(target));
    let kept;
    try {
      kept = await this._installMacPackage(file, a.kind, a.version, keepOld);
    } finally {
      await this._removeCopy(file);
    }
    if (kept) {
      const item = path.basename(target);
      await F.writeJsonAtomic(path.join(backupDir, F.BACKUP_MARK), {
        schema: F.SCHEMA, version: this.version, kind: this.install.kind, item, created_at: this.now().toISOString(),
      });
      await this._recordBackup(backupDir, item).catch((err) => this.log.warn(`selfupdate: recording the backup: ${F.shortError(err)}`));
      prev.backup = F.relativeTo(this.dir, backupDir);
      await F.writeJsonAtomic(this.file('previous'), prev);
    }
    this.ops.releaseLock();
    return this._spawnQuiet('/usr/bin/open', ['-n', target, '--args', UPDATED_ARG]);
  }

  /** _backupInstallation: copy what is replaced into backup/<version>/app. */
  async _backupInstallation() {
    const dir = this.backupDir(this.version);
    const { kind, target } = this.install;
    await fsp.rm(dir, { recursive: true, force: true });
    await fsp.mkdir(path.join(dir, 'app'), { recursive: true });
    let item;
    if (kind === F.KINDS.installer) {
      await fsp.cp(target, path.join(dir, 'app'), { recursive: true, preserveTimestamps: true, verbatimSymlinks: true });
      item = '.';
    } else {
      item = path.basename(target);
      await fsp.copyFile(target, path.join(dir, 'app', item));
    }
    await F.writeJsonAtomic(path.join(dir, F.BACKUP_MARK), {
      schema: F.SCHEMA, version: this.version, kind, item, created_at: this.now().toISOString(),
    });
    await this._recordBackup(dir, item);
    this.log.info(`selfupdate: LectureFlow ${this.version} backed up to ${F.relativeTo(this.dir, dir)}`);
    return F.relativeTo(this.dir, dir);
  }

  /** _recordBackup(dir, item): the tree hash of backup/<version>/app/<item> into the trust file. */
  async _recordBackup(dir, item) {
    const h = await F.treeHash(path.join(dir, 'app', item === '.' ? '' : item));
    await this._updateTrust((t) => {
      t.backups = t.backups || {};
      t.backups[this.version] = {
        dir: F.relativeTo(this.dir, dir), kind: this.install.kind, item, tree: h.sha256, files: h.files, bytes: h.bytes, created_at: this.now().toISOString(),
      };
    });
  }

  /** _snapshotDb: copy the database files into backup/<version>/db; null when there is none. */
  async _snapshotDb() {
    const dir = path.join(this.backupDir(this.version), 'db');
    await fsp.rm(dir, { recursive: true, force: true });
    await fsp.mkdir(dir, { recursive: true });
    let any = false;
    for (const name of F.DB_FILES) {
      try {
        await fsp.copyFile(path.join(this.dataDir, name), path.join(dir, name));
        any = true;
      } catch (err) {
        if (!err || err.code !== 'ENOENT') throw err;
      }
    }
    if (!any) {
      await fsp.rm(dir, { recursive: true, force: true });
      return null;
    }
    return F.relativeTo(this.dir, dir);
  }

  // ---- replacing files ----

  /**
   * _replaceFile(target, source): put a copy of source at target. The
   * running portable executable cannot be overwritten or deleted, but it can
   * be renamed: target → target.lf-old (removed at a later start).
   */
  async _replaceFile(target, source) {
    const incoming = `${target}.lf-new`;
    await fsp.rm(incoming, { force: true });
    await fsp.copyFile(source, incoming).catch((err) => {
      throw notWritable(err, target);
    });
    const aside = await this._freeName(`${target}.lf-old`);
    await F.renameRetry(target, aside);
    try {
      await F.renameRetry(incoming, target);
    } catch (err) {
      await F.renameRetry(aside, target).catch(() => {});
      await fsp.rm(incoming, { force: true }).catch(() => {});
      throw err;
    }
  }

  /**
   * _replaceDir(target, source): Windows installation directory from a
   * backup. The failed one is moved aside first (fails cleanly while one of
   * its programs still runs), the copy goes in, the failed one is removed.
   */
  async _replaceDir(target, source) {
    const aside = `${target}.lf-failed-${stamp(this.now())}`;
    await F.renameRetry(target, aside, { tries: 60, delayMs: 500 });
    try {
      await fsp.cp(source, target, { recursive: true, preserveTimestamps: true, verbatimSymlinks: true });
    } catch (err) {
      await fsp.rm(target, { recursive: true, force: true }).catch(() => {});
      await F.renameRetry(aside, target).catch(() => {});
      throw err;
    }
    await fsp.rm(aside, { recursive: true, force: true }).catch(() => {});
  }

  async _freeName(base) {
    try {
      await fsp.rm(base, { force: true });
      return base;
    } catch {
      return `${base}-${stamp(this.now())}`;
    }
  }

  /**
   * _installMacPackage(file, kind, version, keepOld): unpack the .app of a
   * zip (or copy it out of a dmg) next to the current one, check it, swap.
   * Returns true when the old .app was moved to keepOld.
   */
  async _installMacPackage(file, kind, version, keepOld) {
    const { target } = this.install;
    if (/\/AppTranslocation\//.test(target)) {
      throw new Error('LectureFlow runs from a read-only (translocated) location; move LectureFlow.app to the Applications folder');
    }
    const staging = path.join(path.dirname(target), `.${path.basename(target)}.lf-new-${version}`);
    await fsp.rm(staging, { recursive: true, force: true });
    await fsp.mkdir(staging).catch((err) => {
      throw notWritable(err, target); // e.g. started from the dmg
    });
    try {
      const out = path.join(staging, 'out');
      await fsp.mkdir(out);
      if (kind === F.KINDS.dmg) {
        const mnt = path.join(staging, 'mnt');
        await fsp.mkdir(mnt);
        await this.ops.run('/usr/bin/hdiutil', ['attach', '-nobrowse', '-readonly', '-noautoopen', '-mountpoint', mnt, file]);
        try {
          const mounted = await findApp(mnt);
          await this.ops.run('/usr/bin/ditto', [mounted, path.join(out, path.basename(mounted))]);
        } finally {
          await this.ops.run('/usr/bin/hdiutil', ['detach', mnt, '-force']).catch(() => {});
        }
      } else {
        await this.ops.run('/usr/bin/ditto', ['-x', '-k', file, out]);
      }
      const app = await findApp(out);
      await this._checkMacApp(app, version);
      await this.ops.run('/usr/bin/xattr', ['-dr', 'com.apple.quarantine', app]).catch(() => {});
      await this.ops.run('/usr/bin/codesign', ['--verify', '--deep', '--strict', app]);
      return await this._swapApp(app, keepOld);
    } finally {
      await fsp.rm(staging, { recursive: true, force: true }).catch(() => {});
    }
  }

  async _checkMacApp(app, version) {
    const plist = path.join(app, 'Contents', 'Info.plist');
    const read = async (key) => {
      const { stdout } = await this.ops.run('/usr/bin/plutil', ['-extract', key, 'raw', '-o', '-', plist]);
      return String(stdout).trim();
    };
    const id = await read('CFBundleIdentifier');
    if (id !== F.MAC_BUNDLE_ID) throw new Error(`the package holds ${id || 'an unknown app'}, not LectureFlow`);
    const shown = await read('CFBundleShortVersionString').catch(() => '');
    if (shown && shown !== version) this.log.warn(`selfupdate: the new app says version ${shown}, update.json ${version}`);
  }

  /**
   * _swapApp(incoming, keepOld): incoming .app takes the place of the
   * current one; the current one goes to keepOld (or is removed).
   */
  async _swapApp(incoming, keepOld) {
    const { target } = this.install;
    const aside = path.join(path.dirname(target), `.${path.basename(target)}.lf-old-${stamp(this.now())}`);
    await F.renameRetry(target, aside);
    try {
      await this._moveDir(incoming, target);
    } catch (err) {
      await F.renameRetry(aside, target).catch(() => {});
      throw err;
    }
    if (!keepOld) {
      await fsp.rm(aside, { recursive: true, force: true }).catch(() => {});
      return false;
    }
    try {
      await fsp.rm(keepOld, { recursive: true, force: true });
      await fsp.mkdir(path.dirname(keepOld), { recursive: true });
      await this._moveDir(aside, keepOld);
      return true;
    } catch (err) {
      this.log.warn(`selfupdate: keeping the previous app as a backup: ${F.shortError(err)}`);
      return false;
    }
  }

  async _moveDir(from, to) {
    try {
      await fsp.rename(from, to);
    } catch (err) {
      if (!err || err.code !== 'EXDEV') throw err;
      await this.ops.run('/usr/bin/ditto', [from, to]);
      await fsp.rm(from, { recursive: true, force: true }).catch(() => {});
    }
  }

  async _spawnQuiet(file, args) {
    try {
      return await this.ops.spawnDetached(file, args, {});
    } catch (err) {
      this.log.warn(`selfupdate: starting ${path.basename(file)}: ${F.shortError(err)}`);
      return null;
    }
  }

  // ---- the private copies ----

  /** _removeCopy(copy): remove a private copy's folder (only one the service program made in tmpDir). */
  async _removeCopy(copy) {
    if (!copy) return;
    const dir = path.dirname(copy);
    if (!samePath(path.dirname(dir), this.tmpDir) || !COPY_DIR_RE.test(path.basename(dir))) return;
    await fsp.rm(dir, { recursive: true, force: true }).catch((err) => this.log.warn(`selfupdate: removing ${dir}: ${F.shortError(err)}`));
  }

  /**
   * _cleanupCopies: private copies an earlier start left (an installer runs
   * from its copy while this app quits). Only folders of the copies' name
   * that are older than an installer may take; one still in use cannot be
   * removed and stays for later.
   */
  async _cleanupCopies() {
    let names;
    try {
      names = await fsp.readdir(this.tmpDir);
    } catch {
      return;
    }
    const now = this.now().getTime();
    for (const name of names) {
      if (!COPY_DIR_RE.test(name)) continue;
      const p = path.join(this.tmpDir, name);
      try {
        const st = await fsp.lstat(p);
        if (!st.isDirectory() || now - st.mtimeMs < HELPER_WAIT_MS) continue;
        await fsp.rm(p, { recursive: true, force: true });
      } catch {
        // in use or gone: a later start tries again
      }
    }
  }

  // ---- bookkeeping ----

  async _readPrevious() {
    const read = await F.readJson(this.file('previous'));
    const p = read.value;
    if (!p || p.schema !== F.SCHEMA || !F.parseVersion(p.version)) return null;
    return p;
  }

  _trustFile() {
    return this.privateDir ? path.join(this.privateDir, F.TRUST_FILE) : null;
  }

  /** _readTrust → the trust file ({ schema, install?, backups }); empty when missing or unreadable. */
  async _readTrust() {
    const empty = { schema: F.SCHEMA, backups: {} };
    const f = this._trustFile();
    if (!f) return empty;
    const read = await F.readJson(f);
    const t = read.value;
    if (!t || t.schema !== F.SCHEMA) return empty;
    if (!t.backups || typeof t.backups !== 'object' || Array.isArray(t.backups)) t.backups = {};
    if (t.install && (typeof t.install !== 'object' || typeof t.install.target !== 'string')) delete t.install;
    return t;
  }

  async _updateTrust(change) {
    const f = this._trustFile();
    if (!f) throw new Error('no profile folder');
    const t = await this._readTrust();
    change(t);
    await F.writeJsonAtomic(f, t);
  }

  async _writeResult(a, outcome, error) {
    const r = { version: a.version, from: a.from_version, outcome, at: this.now().toISOString() };
    if (error) r.error = F.shortError(error);
    await F.writeJsonAtomic(this.file('result'), r);
  }

  /** _finish: result.json, applying.json and its record removed, database snapshot and stale backups removed. */
  async _finish(a, outcome, error) {
    await this._writeResult(a, outcome, outcome === 'installed' ? null : error);
    if (outcome !== 'installed') this.log.warn(`selfupdate: ${outcome}: ${a.from_version} → ${a.version}: ${F.shortError(error)}`);
    await F.removeFile(this.file('applying'));
    await this._updateTrust((t) => {
      if (t.install && F.sameVersion(t.install.version, a.version) && F.sameVersion(t.install.from_version, a.from_version)) delete t.install;
    }).catch((err) => this.log.warn(`selfupdate: updating the trust file: ${F.shortError(err)}`));
    await this._pruneBackups().catch((err) => this.log.warn(`selfupdate: removing old backups: ${F.shortError(err)}`));
  }

  /**
   * _pruneBackups: keep only the backup previous.json names, without its
   * database snapshot (which only serves a rollback during the first start);
   * the trust file forgets the others.
   */
  async _pruneBackups() {
    const root = path.join(this.dir, F.BACKUP_DIR);
    let names;
    try {
      names = await fsp.readdir(root);
    } catch {
      names = [];
    }
    const prev = await this._readPrevious();
    const keep = prev && prev.backup ? F.resolveInside(this.dir, prev.backup) : null;
    for (const name of names) {
      const p = path.join(root, name);
      if (keep && samePath(p, keep)) {
        await fsp.rm(path.join(p, 'db'), { recursive: true, force: true });
      } else {
        await fsp.rm(p, { recursive: true, force: true });
      }
    }
    if (!this._trustFile()) return;
    const keepRel = keep ? F.relativeTo(this.dir, keep) : null;
    await this._updateTrust((t) => {
      for (const v of Object.keys(t.backups)) {
        if (!t.backups[v] || t.backups[v].dir !== keepRel) delete t.backups[v];
      }
    });
  }

  /** _cleanupLeftovers: files an earlier installation could not remove yet. */
  async _cleanupLeftovers() {
    const { kind, target } = this.install;
    if (!target) return;
    const dir = path.dirname(target);
    const base = path.basename(target);
    let names;
    try {
      names = await fsp.readdir(dir);
    } catch {
      return;
    }
    const prefixes = kind === F.KINDS.portable ? [`${base}.lf-old`, `${base}.lf-new`]
      : kind === F.KINDS.installer ? [`${base}.lf-failed-`]
        : [`.${base}.lf-old-`, `.${base}.lf-new-`];
    for (const name of names) {
      if (!prefixes.some((p) => name.startsWith(p))) continue;
      await fsp.rm(path.join(dir, name), { recursive: true, force: true }).catch(() => {});
    }
  }
}

/** notWritable: a readable reason when the folder of what is replaced cannot be written. */
function notWritable(err, target) {
  if (!err || !['EACCES', 'EPERM', 'EROFS'].includes(err.code)) return err;
  return new Error(`the folder of ${path.basename(target)} cannot be written (${err.code}); move LectureFlow to a folder you can write to, or install the new version by hand`);
}

async function findApp(dir) {
  const apps = (await fsp.readdir(dir)).filter((n) => /\.app$/i.test(n) && !n.startsWith('.'));
  if (apps.length !== 1) throw new Error(`the package holds ${apps.length} apps, not one`);
  return path.join(dir, apps[0]);
}

// ---- ops ----

/**
 * spawnDetached(file, args, { verbatim }) → pid. verbatim (Windows): the
 * arguments go on the command line exactly as given (NSIS wants "/D=" last
 * and unquoted); the program path is quoted here.
 */
function spawnDetached(file, args, { verbatim = false } = {}) {
  return new Promise((resolve, reject) => {
    const opts = { detached: true, stdio: 'ignore', windowsHide: false };
    if (verbatim && process.platform === 'win32') {
      opts.windowsVerbatimArguments = true;
      opts.argv0 = `"${file}"`;
    }
    let child;
    try {
      child = childProcess.spawn(file, args, opts);
    } catch (err) {
      reject(err);
      return;
    }
    child.once('error', reject);
    child.once('spawn', () => {
      child.unref();
      resolve(child.pid);
    });
  });
}

function runProgram(file, args) {
  return new Promise((resolve, reject) => {
    childProcess.execFile(file, args, { timeout: 10 * 60 * 1000, maxBuffer: 4 << 20, windowsHide: true }, (err, stdout, stderr) => {
      if (err) {
        const detail = String(stderr || '').trim().split(/\r?\n/).pop();
        reject(new Error(`${path.basename(file)} failed${detail ? `: ${detail}` : ''}`));
        return;
      }
      resolve({ stdout, stderr });
    });
  });
}

function pidAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err) {
    return !!err && err.code === 'EPERM';
  }
}

/**
 * electronOps({ app, verifyPending }): the real effects. verifyPending(dir,
 * version, opts) → lib/verifyupdate.runVerify's result: the installed
 * service program's -selfupdate-verify of the updates directory dir. The
 * trust file lives in the shell's profile (userData).
 */
function electronOps({ app, verifyPending }) {
  return {
    spawnDetached,
    run: runProgram,
    verifyPending,
    releaseLock: () => {
      try {
        app.releaseSingleInstanceLock();
      } catch {
        // not holding it
      }
    },
    pidAlive,
    privateDir: () => app.getPath('userData'),
    tmpDir: () => os.tmpdir(),
  };
}

/**
 * recordingOps(markFile, { verifyPending, privateDir, tmpDir }): end-to-end
 * tests of the unpackaged app (LECTUREFLOW_TEST_SELFUPDATE): programs are
 * not started; every effect is appended to markFile as one JSON line. The
 * update check is recorded and then really run (it only reads and makes
 * the private copy; in these tests the service program is the fake
 * service). The trust file goes to privateDir (default: "private" next to
 * markFile), the copies to tmpDir (default: "tmp" next to markFile).
 */
function recordingOps(markFile, { verifyPending, privateDir, tmpDir } = {}) {
  const fs = require('node:fs');
  const record = (entry) => fs.appendFileSync(markFile, `${JSON.stringify({ ...entry, at: Date.now() })}\n`);
  const tmp = tmpDir || path.join(path.dirname(markFile), 'tmp');
  return {
    spawnDetached: async (file, args, opts = {}) => {
      record({ op: 'spawn', file, args, verbatim: !!opts.verbatim });
      return null;
    },
    run: async (file, args) => {
      record({ op: 'run', file, args });
      return { stdout: '' };
    },
    verifyPending: async (dir, version, opts = {}) => {
      record({ op: 'verify', dir, version, previous: !!opts.previous, copyTo: opts.copyTo || '' });
      if (typeof verifyPending !== 'function') return { code: null, stdout: '', stderr: '', timedOut: false, error: 'no update check program' };
      if (opts.copyTo) fs.mkdirSync(opts.copyTo, { recursive: true });
      return verifyPending(dir, version, opts);
    },
    releaseLock: () => record({ op: 'releaseLock' }),
    pidAlive: () => false,
    privateDir: () => privateDir || path.join(path.dirname(markFile), 'private'),
    tmpDir: () => tmp,
  };
}

/**
 * startFailureReason(err): a failed start of the service (server.js
 * StartError) in short English, for result.json.
 */
function startFailureReason(err) {
  if (!err || typeof err.kind !== 'string') return `the service did not start: ${F.shortError(err)}`;
  const reason = err.reason ? `: ${err.reason}` : '';
  switch (err.kind) {
    case 'spawn_failed':
      return `the service program could not be run${reason}`;
    case 'timeout':
      return 'the service did not answer within 30 seconds';
    case 'port_in_use':
      return 'the service found no port to listen on';
    case 'token_rejected':
      return 'the service rejected the access token';
    case 'bad_mode':
      return 'the service did not run in desktop mode';
    default:
      return `the service stopped while starting${err.code !== null && err.code !== undefined ? ` (exit code ${err.code})` : ''}${reason}`;
  }
}

/**
 * testInstallFrom(env, isPackaged) → { install, mark } for end-to-end tests
 * of the unpackaged app: LECTUREFLOW_TEST_SELFUPDATE is
 * {"install": {platform, kind, target, exe}, "mark": "<file>"}; programs are
 * then recorded in the mark file instead of started. Never in a packaged app.
 */
function testInstallFrom(env, isPackaged) {
  if (isPackaged || !env || !env.LECTUREFLOW_TEST_SELFUPDATE) return null;
  let v;
  try {
    v = JSON.parse(env.LECTUREFLOW_TEST_SELFUPDATE);
  } catch {
    return null;
  }
  const i = v && v.install;
  if (!i || typeof v.mark !== 'string' || !path.isAbsolute(v.mark)) return null;
  if (!Object.values(F.KINDS).includes(i.kind) || typeof i.target !== 'string' || !path.isAbsolute(i.target)) return null;
  const kinds = i.kind === F.KINDS.zip || i.kind === F.KINDS.dmg ? [F.KINDS.zip, F.KINDS.dmg] : [i.kind];
  return {
    install: { platform: String(i.platform || ''), kind: i.kind, kinds, target: i.target, exe: String(i.exe || 'LectureFlow.exe') },
    mark: v.mark,
  };
}

module.exports = {
  SelfUpdateApplier,
  electronOps,
  recordingOps,
  spawnDetached,
  startFailureReason,
  testInstallFrom,
  MAX_TRIES,
  RESTORE_ARG,
  UPDATED_ARG,
  HELPER_WAIT_MS,
};
