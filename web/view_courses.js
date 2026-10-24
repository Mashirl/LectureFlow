// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
// #/courses: every course as a card (v1.1 A9 course overview cards, A12.2
// course card menu) and the "add course" entry (the A8 wizard).

import { h, clear, btn, skeleton, preserveFocus } from './dom.js';
import { icon } from './icons.js';
import { courseCard, addCourseCard, cardOf } from './course_card.js';
import { t } from './i18n.js';

export function mountCourses(root, ctx) {
  const el = h('div', { class: 'page page-courses' });
  root.append(el);
  let loaded = ctx.coursesLoaded();

  function render() {
    preserveFocus(() => {
      clear(el);
      const courses = ctx.courses();
      el.append(
        h(
          'header',
          { class: 'page-head' },
          h(
            'div',
            { class: 'page-titles' },
            h('h1', { class: 'page-title' }, t('course.list.title')),
            h('p', { class: 'page-sub' }, loaded ? [t('course.list.count', { n: courses.length }), t('course.head.lectures', { n: courses.reduce((n, c) => n + c.lecture_count, 0) })].join(' · ') : t('course.list.loading')),
          ),
          h('div', { class: 'page-actions' }, btn(t('course.add'), { kind: 'primary', icon: 'plus', onclick: ctx.newCourse, fkey: 'courses:add' })),
        ),
      );
      if (!loaded) {
        el.append(h('div', { class: 'ccards ccards-lg' }, [0, 1, 2].map(() => h('div', { class: 'ccard is-loading' }, skeleton(4)))));
        return;
      }
      if (courses.length === 0) {
        el.append(
          h(
            'div',
            { class: 'onboard' },
            h('div', { class: 'onboard-icon' }, icon('graduation-cap', { size: 30 })),
            h('h2', { class: 'onboard-title' }, t('course.list.empty_title')),
            h('p', { class: 'onboard-text' }, t('course.list.empty_text')),
            btn(t('course.add'), { kind: 'primary', icon: 'plus', onclick: ctx.newCourse }),
          ),
        );
        return;
      }
      el.append(
        h(
          'div',
          { class: 'ccards ccards-lg' },
          courses.map((c) => courseCard(cardOf(c), { onChanged: refresh })),
          addCourseCard(ctx.newCourse),
        ),
        h('p', { class: 'page-foot muted small' }, t('course.list.foot')),
      );
    });
  }

  async function refresh() {
    try {
      await ctx.refreshCourses();
    } catch {
      // shown by the API layer
    }
    loaded = true;
    render();
  }

  render();
  refresh();

  const tick = setInterval(render, 60000); // relative times
  return {
    refresh,
    // The app refetches the course list on `changed course` and calls this.
    onCoursesChanged() {
      loaded = true;
      render();
    },
    destroy() {
      clearInterval(tick);
    },
  };
}
