<!--
 Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
-->

<script setup>
import { ref, onMounted, onUnmounted, watch, nextTick } from 'vue'

const NVIDIA_GREEN = '#76B900'
const NVIDIA_GREEN_DIM = 'rgba(118, 185, 0, 0.15)'
const NVIDIA_GREEN_GLOW = 'rgba(118, 185, 0, 0.3)'

// ── useCountUp composable ──────────────────────────────────────────
function useCountUp(end, duration = 2000) {
  const count = ref(0)
  const started = ref(false)
  const elRef = ref(null)

  onMounted(() => {
    const observer = new IntersectionObserver(
      ([entry]) => { if (entry.isIntersecting) started.value = true },
      { threshold: 0.5 }
    )
    if (elRef.value) observer.observe(elRef.value)
    onUnmounted(() => observer.disconnect())
  })

  watch(started, (val) => {
    if (!val) return
    let startTime = null
    const animate = (timestamp) => {
      if (!startTime) startTime = timestamp
      const progress = Math.min((timestamp - startTime) / duration, 1)
      count.value = Math.floor(progress * end)
      if (progress < 1) requestAnimationFrame(animate)
    }
    requestAnimationFrame(animate)
  })

  return { count, elRef }
}

// ── Fade-in directive (v-fade) ─────────────────────────────────────
const vFade = {
  mounted(el, binding) {
    const delay = binding.value || 0
    el.style.opacity = '0'
    el.style.transform = 'translateY(24px)'
    el.style.transition = `opacity 0.7s ease ${delay}ms, transform 0.7s ease ${delay}ms`

    const timer = setTimeout(() => {
      const observer = new IntersectionObserver(
        ([entry]) => {
          if (entry.isIntersecting) {
            el.style.opacity = '1'
            el.style.transform = 'translateY(0)'
            observer.disconnect()
          }
        },
        { threshold: 0.1, rootMargin: '-40px' }
      )
      observer.observe(el)
    }, 50)

    el._fadeCleanup = () => clearTimeout(timer)
  },
  unmounted(el) {
    if (el._fadeCleanup) el._fadeCleanup()
  }
}

// ── Counter refs ───────────────────────────────────────────────────
const stat66 = useCountUp(66, 1800)
const stat25 = useCountUp(25, 1800)

// ── Scroll-aware nav ───────────────────────────────────────────────
const navScrolled = ref(false)
function handleScroll() {
  navScrolled.value = window.scrollY > 40
}
onMounted(() => window.addEventListener('scroll', handleScroll))
onUnmounted(() => window.removeEventListener('scroll', handleScroll))

// ── Hover helpers (replace inline onMouseEnter / onMouseLeave) ─────
function pipelineEnter(e, accent) {
  e.currentTarget.style.borderColor = 'rgba(118, 185, 0, 0.4)'
  e.currentTarget.style.background = 'rgba(118, 185, 0, 0.06)'
}
function pipelineLeave(e, accent) {
  e.currentTarget.style.borderColor = accent ? 'rgba(118, 185, 0, 0.2)' : 'rgba(255,255,255,0.06)'
  e.currentTarget.style.background = accent ? 'rgba(118, 185, 0, 0.04)' : 'rgba(255,255,255,0.02)'
}
function featureCardEnter(e) {
  e.currentTarget.style.borderColor = 'rgba(118, 185, 0, 0.3)'
  e.currentTarget.style.transform = 'translateY(-2px)'
}
function featureCardLeave(e) {
  e.currentTarget.style.borderColor = 'rgba(255,255,255,0.06)'
  e.currentTarget.style.transform = 'translateY(0)'
}
function navLinkEnter(e) { e.target.style.color = '#fff' }
function navLinkLeave(e) { e.target.style.color = 'rgba(255,255,255,0.6)' }
function greenBtnEnter(e) { e.target.style.opacity = '0.9' }
function greenBtnLeave(e) { e.target.style.opacity = '1' }
function heroBtnEnter(e) { e.target.style.opacity = '0.9'; e.target.style.transform = 'translateY(-1px)' }
function heroBtnLeave(e) { e.target.style.opacity = '1'; e.target.style.transform = 'translateY(0)' }
function outlineBtnEnter(e) { e.currentTarget.style.borderColor = 'rgba(255,255,255,0.35)' }
function outlineBtnLeave(e) { e.currentTarget.style.borderColor = 'rgba(255,255,255,0.15)' }
function roleLinkEnter(e) {
  e.currentTarget.style.borderColor = 'rgba(118, 185, 0, 0.3)'
  e.currentTarget.style.background = 'rgba(118, 185, 0, 0.04)'
}
function roleLinkLeave(e) {
  e.currentTarget.style.borderColor = 'rgba(255,255,255,0.06)'
  e.currentTarget.style.background = 'rgba(255,255,255,0.02)'
}

// ── Data arrays ────────────────────────────────────────────────────
const workflowSteps = [
  { label: 'Recipe', desc: 'Generate an optimized, version-locked configuration for your specific environment.' },
  { label: 'Bundle', desc: 'Convert the recipe into deployment-ready artifacts for Helm, ArgoCD, or OCI images.' },
  { label: 'Deploy', desc: 'Apply through your existing CD pipeline. No new tooling required.' },
  { label: 'Validate', desc: 'Verify deployment, performance, and conformance checks against your live cluster.' },
]

const featureCards = [
  { title: 'Optimized', description: 'Tuned for a specific combination of hardware, cloud, OS, and workload intent.', icon: 'settings', delay: 0 },
  { title: 'Validated', description: 'Passes automated constraint and compatibility checks before publishing.', icon: 'check-circle', delay: 80 },
  { title: 'Reproducible', description: 'Same inputs produce identical deployments every time.', icon: 'copy', delay: 160 },
  { title: 'Composable', description: 'Recipes compose from layered overlays: base defaults, cloud, accelerator, OS, and workload-specific tuning.', icon: 'layers', delay: 240 },
  { title: 'Secure', description: 'SLSA Level 3 provenance, SPDX SBOMs, and Sigstore cosign attestations on every release.', icon: 'shield', delay: 320 },
  { title: 'Standards Based', description: 'Built on existing standards. Recipes are YAML, bundles produce Helm charts, and deployment works through Helm, ArgoCD, or any CD pipeline.', icon: 'git-branch', delay: 400 },
]

const environmentCols = [
  { label: 'Cloud or On-Premises', items: 'Amazon EKS, GKE, self-managed' },
  { label: 'GPUs', items: 'NVIDIA H100, GB200' },
  { label: 'Workloads', items: 'Training (Kubeflow), Inference (Dynamo)' },
]

const roleLinks = [
  { role: 'Users', desc: 'Install, configure, and operate AICR in your environment.', link: 'https://aicr.dgxc.io/docs/user/' },
  { role: 'Integrators', desc: 'Embed AICR in automation pipelines and CI/CD workflows.', link: 'https://aicr.dgxc.io/docs/integrator/' },
  { role: 'Contributors', desc: 'Understand internals, add components, and extend the platform.', link: 'https://aicr.dgxc.io/docs/contributor/' },
]
</script>

<template>
  <div :style="{
    background: '#0a0a0a',
    color: '#fff',
    minHeight: '100vh',
    fontFamily: `'DM Sans', sans-serif`,
    overflowX: 'hidden',
  }">
    <!-- Google Fonts -->
    <Teleport to="head">
      <link href="https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@400;500;600;700&family=DM+Sans:wght@400;500;600&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet" />
    </Teleport>

    <!-- Nav -->
    <nav :style="{
      position: 'fixed',
      top: 0,
      left: 0,
      right: 0,
      zIndex: 100,
      padding: '0 48px',
      height: '64px',
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'space-between',
      background: navScrolled ? 'rgba(10,10,10,0.92)' : 'transparent',
      backdropFilter: navScrolled ? 'blur(16px)' : 'none',
      borderBottom: navScrolled ? '1px solid rgba(255,255,255,0.06)' : '1px solid transparent',
      transition: 'all 0.3s ease',
    }">
      <div :style="{ display: 'flex', alignItems: 'center', gap: '12px' }">
        <div :style="{
          width: '8px',
          height: '8px',
          borderRadius: '50%',
          background: NVIDIA_GREEN,
          boxShadow: `0 0 12px ${NVIDIA_GREEN_GLOW}`,
        }" />
        <span :style="{
          fontFamily: `'Space Grotesk', sans-serif`,
          fontSize: '16px',
          fontWeight: 600,
          letterSpacing: '0.5px',
        }">AI Cluster Runtime</span>
      </div>
      <div :style="{ display: 'flex', alignItems: 'center', gap: '32px' }">
        <a href="https://aicr.dgxc.io/docs/" :style="{ color: 'rgba(255,255,255,0.6)', textDecoration: 'none', fontSize: '14px', fontWeight: 500, transition: 'color 0.2s' }"
          @mouseenter="navLinkEnter" @mouseleave="navLinkLeave">Docs</a>
        <a href="https://github.com/NVIDIA/aicr" :style="{ color: 'rgba(255,255,255,0.6)', textDecoration: 'none', fontSize: '14px', fontWeight: 500, transition: 'color 0.2s' }"
          @mouseenter="navLinkEnter" @mouseleave="navLinkLeave">GitHub</a>
        <a href="https://github.com/NVIDIA/aicr" :style="{
          background: NVIDIA_GREEN,
          color: '#000',
          padding: '8px 20px',
          borderRadius: '8px',
          fontSize: '13px',
          fontWeight: 600,
          textDecoration: 'none',
          transition: 'opacity 0.2s',
        }" @mouseenter="greenBtnEnter" @mouseleave="greenBtnLeave">Get Started</a>
      </div>
    </nav>

    <!-- Hero -->
    <section :style="{
      position: 'relative',
      minHeight: '100vh',
      display: 'flex',
      flexDirection: 'column',
      justifyContent: 'center',
      padding: '120px 48px 80px',
      overflow: 'hidden',
    }">
      <!-- Background grid effect -->
      <div :style="{
        position: 'absolute',
        inset: 0,
        backgroundImage: `linear-gradient(rgba(118,185,0,0.03) 1px, transparent 1px), linear-gradient(90deg, rgba(118,185,0,0.03) 1px, transparent 1px)`,
        backgroundSize: '64px 64px',
        maskImage: 'radial-gradient(ellipse 70% 60% at 50% 40%, black, transparent)',
      }" />
      <!-- Glow -->
      <div :style="{
        position: 'absolute',
        top: '-20%',
        right: '-10%',
        width: '800px',
        height: '800px',
        background: 'radial-gradient(circle, rgba(118,185,0,0.08) 0%, transparent 60%)',
        pointerEvents: 'none',
      }" />

      <div :style="{ position: 'relative', maxWidth: '860px' }">
        <div v-fade>
          <div :style="{
            fontFamily: `'JetBrains Mono', monospace`,
            fontSize: '12px',
            color: NVIDIA_GREEN,
            letterSpacing: '3px',
            textTransform: 'uppercase',
            marginBottom: '24px',
          }">NVIDIA Open Source</div>
        </div>

        <div v-fade="100">
          <h1 :style="{
            fontFamily: `'Space Grotesk', sans-serif`,
            fontSize: 'clamp(48px, 6vw, 84px)',
            fontWeight: 700,
            lineHeight: 1.0,
            margin: '0 0 20px 0',
            letterSpacing: '-2px',
          }">AI Cluster Runtime</h1>
        </div>

        <div v-fade="180">
          <p :style="{
            fontFamily: `'Space Grotesk', sans-serif`,
            fontSize: 'clamp(20px, 2.5vw, 28px)',
            fontWeight: 500,
            lineHeight: 1.35,
            color: 'rgba(255,255,255,0.7)',
            maxWidth: '640px',
            margin: '0 0 24px 0',
          }">
            Tooling for optimized, validated, and reproducible{{ ' ' }}<span :style="{ color: NVIDIA_GREEN }">GPU-accelerated Kubernetes.</span>
          </p>
        </div>

        <div v-fade="260">
          <p :style="{
            fontSize: '17px',
            lineHeight: 1.7,
            color: 'rgba(255,255,255,0.45)',
            maxWidth: '560px',
            margin: '0 0 40px 0',
          }">
            AICR makes it easy to stand up GPU-accelerated Kubernetes clusters
            with version-locked recipes you can deploy anywhere.
          </p>
        </div>

        <div v-fade="300">
          <div :style="{ display: 'flex', gap: '16px', flexWrap: 'wrap' }">
            <a href="https://aicr.dgxc.io/docs/" :style="{
              background: NVIDIA_GREEN,
              color: '#000',
              padding: '14px 32px',
              borderRadius: '10px',
              fontSize: '15px',
              fontWeight: 600,
              textDecoration: 'none',
              transition: 'opacity 0.2s, transform 0.2s',
              display: 'inline-block',
            }" @mouseenter="heroBtnEnter" @mouseleave="heroBtnLeave">Get Started</a>
            <a href="https://github.com/NVIDIA/aicr" :style="{
              background: 'transparent',
              color: '#fff',
              padding: '14px 32px',
              borderRadius: '10px',
              fontSize: '15px',
              fontWeight: 500,
              textDecoration: 'none',
              border: '1px solid rgba(255,255,255,0.15)',
              transition: 'border-color 0.2s',
              display: 'inline-block',
            }" @mouseenter="outlineBtnEnter" @mouseleave="outlineBtnLeave">GitHub</a>
          </div>
        </div>

        <div v-fade="450">
          <div :style="{ marginTop: '56px', display: 'inline-block' }">
            <div :style="{
              fontFamily: `'JetBrains Mono', monospace`,
              fontSize: '11px',
              color: 'rgba(255,255,255,0.3)',
              letterSpacing: '1.5px',
              textTransform: 'uppercase',
              marginBottom: '8px',
            }">Install AICR</div>
            <div :style="{
              fontFamily: `'JetBrains Mono', monospace`,
              fontSize: '13px',
              background: 'rgba(255,255,255,0.03)',
              border: '1px solid rgba(255,255,255,0.06)',
              borderRadius: '10px',
              padding: '16px 24px',
              display: 'flex',
              flexDirection: 'column',
              gap: '6px',
            }">
              <div><span :style="{ color: 'rgba(255,255,255,0.35)' }">$</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">brew tap</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">NVIDIA/aicr</span></div>
              <div><span :style="{ color: 'rgba(255,255,255,0.35)' }">$</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">brew install</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">aicr</span></div>
              <div :style="{ margin: '6px 0', color: 'rgba(255,255,255,0.2)', fontSize: '11px' }">or</div>
              <div><span :style="{ color: 'rgba(255,255,255,0.35)' }">$</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">curl -sfL</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">https://raw.githubusercontent.com/NVIDIA/aicr/main/install</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">| bash -s --</span></div>
            </div>
          </div>
        </div>
      </div>
    </section>

    <!-- Problem / Why We Built This -->
    <section :style="{
      padding: '80px 48px',
      borderTop: '1px solid rgba(255,255,255,0.04)',
    }">
      <div :style="{ maxWidth: '1100px', margin: '0 auto' }">
        <div v-fade>
          <div :style="{
            fontFamily: `'JetBrains Mono', monospace`,
            fontSize: '11px',
            color: NVIDIA_GREEN,
            letterSpacing: '3px',
            textTransform: 'uppercase',
            marginBottom: '20px',
          }">Why We Built This</div>
        </div>

        <div :style="{ display: 'flex', gap: '80px', flexWrap: 'wrap', alignItems: 'flex-start' }">
          <div v-fade="100" :style="{ flex: '1 1 480px' }">
            <div :style="{ flex: '1 1 480px' }">
              <h2 :style="{
                fontFamily: `'Space Grotesk', sans-serif`,
                fontSize: 'clamp(28px, 3vw, 40px)',
                fontWeight: 700,
                lineHeight: 1.15,
                margin: '0 0 24px 0',
              }">
                Running GPU-accelerated Kubernetes
                <br />
                <span :style="{ color: 'rgba(255,255,255,0.35)' }">clusters reliably is hard.</span>
              </h2>
              <p :style="{
                fontSize: '16px',
                lineHeight: 1.75,
                color: 'rgba(255,255,255,0.5)',
                margin: '0 0 20px 0',
                maxWidth: '520px',
              }">
                Small differences in kernel versions, drivers, container runtimes, operators,
                and Kubernetes releases can cause failures that are difficult to diagnose
                and expensive to reproduce.
              </p>
              <p :style="{
                fontSize: '16px',
                lineHeight: 1.75,
                color: 'rgba(255,255,255,0.5)',
                margin: 0,
                maxWidth: '520px',
              }">
                Historically, this knowledge has lived in internal validation pipelines
                and runbooks. AI Cluster Runtime makes it available to everyone.
              </p>
            </div>
          </div>

          <div v-fade="250">
            <div :style="{
              flex: '0 0 auto',
              display: 'flex',
              flexDirection: 'column',
              gap: '24px',
            }">
              <div :ref="(el) => { stat66.elRef.value = el }" :style="{
                background: 'rgba(255,255,255,0.02)',
                border: '1px solid rgba(255,255,255,0.06)',
                borderRadius: '16px',
                padding: '36px 44px',
                textAlign: 'center',
              }">
                <div :style="{
                  fontFamily: `'Space Grotesk', sans-serif`,
                  fontSize: '56px',
                  fontWeight: 700,
                  color: NVIDIA_GREEN,
                  lineHeight: 1,
                }">{{ stat66.count.value }}%</div>
                <div :style="{
                  fontSize: '13px',
                  color: 'rgba(255,255,255,0.4)',
                  marginTop: '8px',
                  maxWidth: '180px',
                }">of organizations run AI workloads on Kubernetes</div>
              </div>
              <div :ref="(el) => { stat25.elRef.value = el }" :style="{
                background: 'rgba(255,255,255,0.02)',
                border: '1px solid rgba(118,185,0,0.15)',
                borderRadius: '16px',
                padding: '36px 44px',
                textAlign: 'center',
              }">
                <div :style="{
                  fontFamily: `'Space Grotesk', sans-serif`,
                  fontSize: '56px',
                  fontWeight: 700,
                  color: '#fff',
                  lineHeight: 1,
                }">~{{ stat25.count.value }}%</div>
                <div :style="{
                  fontSize: '13px',
                  color: 'rgba(255,255,255,0.4)',
                  marginTop: '8px',
                  maxWidth: '180px',
                }">are fully in production</div>
              </div>
              <div :style="{
                fontFamily: `'JetBrains Mono', monospace`,
                fontSize: '11px',
                color: 'rgba(255,255,255,0.25)',
                textAlign: 'right',
              }">Source: CNCF Survey</div>
            </div>
          </div>
        </div>
      </div>
    </section>

    <!-- How It Works -->
    <section :style="{
      padding: '80px 48px',
      background: 'rgba(255,255,255,0.01)',
      borderTop: '1px solid rgba(255,255,255,0.04)',
    }">
      <div :style="{ maxWidth: '1100px', margin: '0 auto' }">
        <div v-fade>
          <div :style="{
            fontFamily: `'JetBrains Mono', monospace`,
            fontSize: '11px',
            color: NVIDIA_GREEN,
            letterSpacing: '3px',
            textTransform: 'uppercase',
            marginBottom: '20px',
          }">How It Works</div>
          <h2 :style="{
            fontFamily: `'Space Grotesk', sans-serif`,
            fontSize: 'clamp(28px, 3vw, 40px)',
            fontWeight: 700,
            lineHeight: 1.15,
            margin: '0 0 16px 0',
          }">Define a Working GPU Kubernetes Cluster in Minutes</h2>
          <p :style="{
            fontSize: '16px',
            lineHeight: 1.7,
            color: 'rgba(255,255,255,0.45)',
            maxWidth: '580px',
            margin: '0 0 56px 0',
          }">
            You describe your target (cloud, GPU, OS, workload intent), and AICR
            generates a version-locked configuration you can deploy through your
            existing pipeline.
          </p>
        </div>

        <div v-fade="100">
          <div :style="{
            margin: '0 0 56px 0',
            borderRadius: '12px',
            overflow: 'hidden',
            border: '1px solid rgba(255,255,255,0.08)',
          }">
            <img src="/images/aicr-end-to-end.png" alt="AICR end-to-end workflow: Ingest, Recipe Generation, Recipe, Bundling, Deploy, Validate" style="width: 100%; display: block;" />
          </div>
        </div>

        <div v-fade="200">
          <div :style="{
            display: 'grid',
            gridTemplateColumns: 'repeat(4, 1fr)',
            gap: '32px',
            marginTop: '36px',
          }">
            <div v-for="(item, i) in workflowSteps" :key="i">
              <div :style="{
                fontFamily: `'Space Grotesk', sans-serif`,
                fontSize: '15px',
                fontWeight: 600,
                color: '#fff',
                marginBottom: '6px',
              }">{{ item.label }}</div>
              <div :style="{
                fontFamily: `'DM Sans', sans-serif`,
                fontSize: '13px',
                lineHeight: 1.6,
                color: 'rgba(255,255,255,0.4)',
              }">{{ item.desc }}</div>
            </div>
          </div>
        </div>
      </div>
    </section>

    <!-- Features / Recipes -->
    <section :style="{
      padding: '80px 48px',
      borderTop: '1px solid rgba(255,255,255,0.04)',
    }">
      <div :style="{ maxWidth: '1100px', margin: '0 auto' }">
        <div v-fade>
          <div :style="{
            fontFamily: `'JetBrains Mono', monospace`,
            fontSize: '11px',
            color: NVIDIA_GREEN,
            letterSpacing: '3px',
            textTransform: 'uppercase',
            marginBottom: '20px',
          }">Recipes</div>
          <h2 :style="{
            fontFamily: `'Space Grotesk', sans-serif`,
            fontSize: 'clamp(28px, 3vw, 40px)',
            fontWeight: 700,
            margin: '0 0 40px 0',
          }">Every AICR recipe is</h2>
        </div>

        <div :style="{
          display: 'grid',
          gridTemplateColumns: 'repeat(3, 1fr)',
          gap: '16px',
        }">
          <div v-for="card in featureCards" :key="card.title" v-fade="card.delay">
            <div :style="{
              padding: '32px 28px',
              background: 'rgba(255,255,255,0.02)',
              border: '1px solid rgba(255,255,255,0.06)',
              borderRadius: '12px',
              transition: 'border-color 0.3s, transform 0.2s',
              cursor: 'default',
              height: '100%',
              boxSizing: 'border-box',
            }" @mouseenter="featureCardEnter" @mouseleave="featureCardLeave">
              <div :style="{ fontSize: '28px', marginBottom: '16px' }">
                <!-- Settings / gear icon -->
                <svg v-if="card.icon === 'settings'" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" :stroke="NVIDIA_GREEN" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z"/><circle cx="12" cy="12" r="3"/></svg>
                <!-- CheckCircle icon -->
                <svg v-else-if="card.icon === 'check-circle'" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" :stroke="NVIDIA_GREEN" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg>
                <!-- Copy icon -->
                <svg v-else-if="card.icon === 'copy'" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" :stroke="NVIDIA_GREEN" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/></svg>
                <!-- Layers icon -->
                <svg v-else-if="card.icon === 'layers'" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" :stroke="NVIDIA_GREEN" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/></svg>
                <!-- Shield icon -->
                <svg v-else-if="card.icon === 'shield'" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" :stroke="NVIDIA_GREEN" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg>
                <!-- GitBranch icon -->
                <svg v-else-if="card.icon === 'git-branch'" xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" fill="none" :stroke="NVIDIA_GREEN" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="6" y1="3" x2="6" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/></svg>
              </div>
              <h3 :style="{
                fontFamily: `'Space Grotesk', sans-serif`,
                fontSize: '18px',
                fontWeight: 600,
                color: '#fff',
                margin: '0 0 10px 0',
              }">{{ card.title }}</h3>
              <p :style="{
                fontFamily: `'DM Sans', sans-serif`,
                fontSize: '14px',
                lineHeight: 1.65,
                color: 'rgba(255,255,255,0.5)',
                margin: 0,
              }">{{ card.description }}</p>
            </div>
          </div>
        </div>
      </div>
    </section>

    <!-- Supported Environments + Open Source -->
    <section :style="{
      padding: '80px 48px',
      borderTop: '1px solid rgba(255,255,255,0.04)',
      background: 'rgba(255,255,255,0.01)',
    }">
      <div :style="{ maxWidth: '1100px', margin: '0 auto' }">
        <!-- Supported Environments -->
        <div v-fade>
          <div :style="{
            fontFamily: `'JetBrains Mono', monospace`,
            fontSize: '11px',
            color: NVIDIA_GREEN,
            letterSpacing: '3px',
            textTransform: 'uppercase',
            marginBottom: '20px',
          }">Supported Environments</div>
          <h2 :style="{
            fontFamily: `'Space Grotesk', sans-serif`,
            fontSize: 'clamp(28px, 3vw, 40px)',
            fontWeight: 700,
            lineHeight: 1.15,
            margin: '0 0 24px 0',
          }">Configure any Kubernetes cluster.</h2>
          <p :style="{
            fontSize: '16px',
            lineHeight: 1.75,
            color: 'rgba(255,255,255,0.5)',
            maxWidth: '640px',
            margin: '0 0 40px 0',
          }">
            AICR generates recipes for managed or self-hosted Kubernetes deployments. Current recipes are optimized for EKS, GKE, and self-managed clusters with
            H100 and GB200 GPUs on Ubuntu and COS. Support for additional environments and
            accelerators is on the roadmap.
          </p>
        </div>

        <div v-fade="100">
          <div :style="{
            display: 'grid',
            gridTemplateColumns: 'repeat(3, 1fr)',
            gap: '16px',
            marginBottom: '56px',
          }">
            <div v-for="(col, i) in environmentCols" :key="i" :style="{
              padding: '24px 28px',
              background: 'rgba(255,255,255,0.02)',
              border: '1px solid rgba(255,255,255,0.06)',
              borderRadius: '12px',
            }">
              <div :style="{
                fontFamily: `'Space Grotesk', sans-serif`,
                fontSize: '15px',
                fontWeight: 600,
                color: '#fff',
                marginBottom: '8px',
              }">{{ col.label }}</div>
              <div :style="{
                fontSize: '14px',
                lineHeight: 1.6,
                color: 'rgba(255,255,255,0.4)',
              }">{{ col.items }}</div>
            </div>
          </div>
        </div>

        <!-- Contributing / Open Source -->
        <div v-fade="150">
          <div :style="{
            fontFamily: `'JetBrains Mono', monospace`,
            fontSize: '11px',
            color: NVIDIA_GREEN,
            letterSpacing: '3px',
            textTransform: 'uppercase',
            marginBottom: '20px',
          }">Open Source</div>
          <h2 :style="{
            fontFamily: `'Space Grotesk', sans-serif`,
            fontSize: 'clamp(28px, 3vw, 40px)',
            fontWeight: 700,
            lineHeight: 1.15,
            margin: '0 0 24px 0',
          }">
            Don't see your environment?
            <span :style="{ color: 'rgba(255,255,255,0.35)' }"> Add it.</span>
          </h2>
          <p :style="{
            fontSize: '16px',
            lineHeight: 1.75,
            color: 'rgba(255,255,255,0.5)',
            margin: '0 0 20px 0',
            maxWidth: '640px',
          }">
            AI Cluster Runtime is Apache 2.0. We welcome contributions from CSPs, OEMs,
            platform teams, and individual operators: new recipes, bundler formats,
            validation checks, or bug reports.
          </p>
          <p :style="{
            fontSize: '16px',
            lineHeight: 1.75,
            color: 'rgba(255,255,255,0.5)',
            margin: '0 0 36px 0',
            maxWidth: '640px',
          }">
            Copy an existing overlay, update the criteria and component
            configuration, run <code :style="{ fontFamily: `'JetBrains Mono', monospace`, fontSize: '13px', color: NVIDIA_GREEN, background: 'rgba(118,185,0,0.1)', padding: '2px 6px', borderRadius: '4px' }">make qualify</code>,
            and open a PR.
          </p>
          <div :style="{ display: 'flex', gap: '16px', flexWrap: 'wrap' }">
            <a href="/docs/contributor/" :style="{
              background: NVIDIA_GREEN,
              color: '#000',
              padding: '14px 32px',
              borderRadius: '10px',
              fontSize: '15px',
              fontWeight: 600,
              textDecoration: 'none',
              transition: 'opacity 0.2s',
              display: 'inline-block',
            }" @mouseenter="greenBtnEnter" @mouseleave="greenBtnLeave">Contributor Docs</a>
            <a href="https://github.com/NVIDIA/aicr" :style="{
              background: 'transparent',
              color: '#fff',
              padding: '14px 32px',
              borderRadius: '10px',
              fontSize: '15px',
              fontWeight: 500,
              textDecoration: 'none',
              border: '1px solid rgba(255,255,255,0.15)',
              transition: 'border-color 0.2s',
              display: 'inline-block',
            }" @mouseenter="outlineBtnEnter" @mouseleave="outlineBtnLeave">GitHub</a>
          </div>
        </div>
      </div>
    </section>

    <!-- CTA / Quick Start -->
    <section :style="{
      padding: '80px 48px',
      borderTop: '1px solid rgba(255,255,255,0.04)',
      textAlign: 'center',
    }">
      <div v-fade>
        <div :style="{ maxWidth: '900px', margin: '0 auto' }">
          <h2 :style="{
            fontFamily: `'Space Grotesk', sans-serif`,
            fontSize: 'clamp(28px, 3.5vw, 48px)',
            fontWeight: 700,
            lineHeight: 1.1,
            margin: '0 0 20px 0',
          }">Quick Start</h2>
          <p :style="{
            fontSize: '17px',
            color: 'rgba(255,255,255,0.45)',
            marginBottom: '40px',
          }">
            Install and generate your first recipe in under two minutes.
          </p>

          <div :style="{
            fontFamily: `'JetBrains Mono', monospace`,
            fontSize: '14px',
            background: 'rgba(255,255,255,0.03)',
            border: '1px solid rgba(255,255,255,0.08)',
            borderRadius: '12px',
            padding: '24px 32px',
            textAlign: 'left',
            marginBottom: '40px',
            lineHeight: 2.2,
          }">
            <div><span :style="{ color: 'rgba(255,255,255,0.3)' }">$</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">brew tap</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">NVIDIA/aicr</span></div>
            <div><span :style="{ color: 'rgba(255,255,255,0.3)' }">$</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">brew install</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">aicr</span></div>
            <div :style="{ margin: '4px 0', color: 'rgba(255,255,255,0.2)', fontSize: '11px' }">or</div>
            <div><span :style="{ color: 'rgba(255,255,255,0.3)' }">$</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">curl -sfL</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">https://raw.githubusercontent.com/NVIDIA/aicr/main/install</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">| bash -s --</span></div>
            <div :style="{ margin: '4px 0', color: 'rgba(255,255,255,0.2)', fontSize: '11px' }">then</div>
            <div><span :style="{ color: 'rgba(255,255,255,0.3)' }">$</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">aicr recipe</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">--service</span> <span :style="{ color: 'rgba(255,255,255,0.5)' }">eks</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">--accelerator</span> <span :style="{ color: 'rgba(255,255,255,0.5)' }">h100</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">--intent</span> <span :style="{ color: 'rgba(255,255,255,0.5)' }">training</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">--output</span> <span :style="{ color: 'rgba(255,255,255,0.5)' }">recipe.yaml</span></div>
            <div><span :style="{ color: 'rgba(255,255,255,0.3)' }">$</span> <span :style="{ color: 'rgba(255,255,255,0.7)' }">aicr bundle</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">--recipe</span> <span :style="{ color: 'rgba(255,255,255,0.5)' }">recipe.yaml</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">--deployer</span> <span :style="{ color: 'rgba(255,255,255,0.5)' }">helm</span> <span :style="{ color: 'rgba(118,185,0,0.7)' }">--output</span> <span :style="{ color: 'rgba(255,255,255,0.5)' }">./bundle</span></div>
          </div>

          <div :style="{
            display: 'grid',
            gridTemplateColumns: 'repeat(3, 1fr)',
            gap: '16px',
          }">
            <a v-for="(item, i) in roleLinks" :key="i" :href="item.link" :style="{
              display: 'block',
              padding: '24px 28px',
              background: 'rgba(255,255,255,0.02)',
              border: '1px solid rgba(255,255,255,0.06)',
              borderRadius: '12px',
              textDecoration: 'none',
              textAlign: 'left',
              transition: 'border-color 0.3s, background 0.3s',
            }" @mouseenter="roleLinkEnter" @mouseleave="roleLinkLeave">
              <div :style="{
                fontFamily: `'Space Grotesk', sans-serif`,
                fontSize: '16px',
                fontWeight: 600,
                color: '#fff',
                marginBottom: '4px',
              }">{{ item.role }} →</div>
              <div :style="{
                fontSize: '13px',
                color: 'rgba(255,255,255,0.4)',
              }">{{ item.desc }}</div>
            </a>
          </div>
        </div>
      </div>
    </section>

    <!-- Footer -->
    <footer :style="{
      padding: '40px 48px',
      borderTop: '1px solid rgba(255,255,255,0.04)',
      display: 'flex',
      justifyContent: 'space-between',
      alignItems: 'center',
      flexWrap: 'wrap',
      gap: '16px',
    }">
      <div :style="{ display: 'flex', alignItems: 'center', gap: '8px' }">
        <div :style="{
          width: '6px',
          height: '6px',
          borderRadius: '50%',
          background: NVIDIA_GREEN,
        }" />
        <span :style="{
          fontFamily: `'Space Grotesk', sans-serif`,
          fontSize: '13px',
          color: 'rgba(255,255,255,0.4)',
        }">NVIDIA AI Cluster Runtime</span>
      </div>
      <div :style="{
        fontFamily: `'JetBrains Mono', monospace`,
        fontSize: '11px',
        color: 'rgba(255,255,255,0.2)',
      }">Apache License 2.0</div>
    </footer>
  </div>
</template>

<style scoped>
/* Reset VitePress defaults that might interfere with this standalone landing page */
:deep(.VPDoc),
:deep(.VPContent) {
  padding: 0 !important;
  max-width: none !important;
}

/* Ensure the landing page fills the viewport and overrides any VitePress page chrome */
div {
  box-sizing: border-box;
}
</style>
