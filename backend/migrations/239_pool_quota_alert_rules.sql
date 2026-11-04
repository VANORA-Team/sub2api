-- Account-pool quota alerts use the existing operations email configuration.
-- Thresholds are editable from the alert-rule page.
INSERT INTO ops_alert_rules (
    name, description, enabled, metric_type, operator, threshold,
    window_minutes, sustained_minutes, severity, notify_email, cooldown_minutes,
    created_at, updated_at
) VALUES
    ('号池额度不足', '已监控账号接近额度上限，请检查号池并及时补号。', true,
     'pool_quota_low_accounts', '>', 0, 1, 2, 'P1', true, 60, NOW(), NOW()),
    ('号池余额缓冲不足', '账号已知美元和人民币余额的缓冲均不足，请检查并及时补充。', true,
     'pool_balance_buffer_ratio', '<=', 1, 1, 2, 'P1', true, 60, NOW(), NOW()),
    ('号池美元余额不足', '已知上游美元余额较低，请检查并及时补充。', false,
     'pool_min_balance_usd', '<=', 5, 1, 2, 'P1', true, 60, NOW(), NOW()),
    ('号池人民币余额不足', '已知上游人民币余额较低，请检查并及时补充。', false,
     'pool_min_balance_cny', '<=', 30, 1, 2, 'P1', true, 60, NOW(), NOW()),
    ('号池额度采样异常', '号池中有账号的额度未知或采样过期，请检查采样和账号配置。', true,
     'pool_quota_unknown_accounts', '>', 0, 1, 30, 'P2', true, 120, NOW(), NOW())
ON CONFLICT (name) DO NOTHING;
