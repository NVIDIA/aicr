/**
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import './custom.css'

export default {
  extends: DefaultTheme,
  enhanceApp({ app, router }) {
    // Hide VitePress chrome (nav, sidebar, footer) on the landing page
    if (typeof window !== 'undefined') {
      const updateLayout = () => {
        const isHome = window.location.pathname === '/' || window.location.pathname === '/index.html'
        document.documentElement.classList.toggle('landing-page', isHome)
      }
      router.onAfterRouteChanged = updateLayout
      // Run on initial load
      setTimeout(updateLayout, 0)
    }
  },
} satisfies Theme
