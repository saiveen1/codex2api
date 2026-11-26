export function normalizePlanType(planType?: string): string {
  const raw = (planType || '').toLowerCase().trim();
  if (['prolite', 'pro_lite', 'pro-lite', 'promax', 'pro_max', 'pro-max'].includes(raw)) return 'pro';
  if (['ent26', 'enterprise_cbp_usage_based', 'enterprise_cbp_automation'].includes(raw)) return 'enterprise';
  if (raw === 'edu_plus' || raw === 'edu_pro') return 'edu';
  return raw;
}

// Match the Codex backend's paid-plan families. A plan alone is not an exhausted window.
export function isPremiumUsagePlan(planType?: string): boolean {
  const plan = normalizePlanType(planType);
  return ['edu', 'education', 'k12', 'go'].includes(plan) || plan.includes('plus') ||
    plan.startsWith('pro') || plan.startsWith('team') || plan.includes('enterprise') || plan.includes('business');
}
