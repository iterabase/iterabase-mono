-- HOR-459: reverse the coworker presentation to the previous
-- {workflowTitle, personaName} shape. The English title and the coworker name
-- are kept; the mark, role, summary, lists and Portuguese title have no
-- representation in the old shape and are dropped.

UPDATE workflow.definitions
SET presentation = jsonb_build_object(
        'workflowTitle', COALESCE(presentation->'title'->>'en', ''),
        'personaName', COALESCE(presentation->'coworker'->>'name', ''))
WHERE presentation ? 'coworker';

UPDATE workflow.definitions
SET spec_json = jsonb_set(spec_json, '{presentation}', presentation)
WHERE spec_json ? 'presentation'
  AND spec_json->'presentation' ? 'coworker';

UPDATE work.attempts
SET presentation_snapshot = jsonb_build_object(
        'workflowTitle', COALESCE(presentation_snapshot->'title'->>'en', ''),
        'personaName', COALESCE(presentation_snapshot->'coworker'->>'name', ''))
WHERE presentation_snapshot ? 'coworker';
