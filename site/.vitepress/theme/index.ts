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
