ALTER TABLE deployments
    ADD COLUMN advanced_fingerprint TEXT;

UPDATE deployments
SET advanced_fingerprint = 'firefox'
WHERE deployment_kind = 'ADVANCED_BALANCER'
  AND advanced_fingerprint IS NULL;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_advanced_fingerprint_check CHECK (
        deployment_kind <> 'ADVANCED_BALANCER'
		OR (
			advanced_fingerprint IS NOT NULL
			AND advanced_fingerprint IN (
				'chrome', 'firefox', 'safari', 'ios', 'android',
				'edge', 'qq', 'random', 'randomized'
			)
		)
    );
