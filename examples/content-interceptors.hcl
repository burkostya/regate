surface "training" {
  path = "/mcp/training"

  resource "guide" {
    uri       = "regate://training/guide"
    mime_type = "text/plain"
    text      = "Training contacts: training@example.com"
  }

  prompt "review" {
    text = "Review the following topic"
    arguments = [{ name = "topic", required = true }]
    input_schema = {
      type = "object"
      properties = {
        arguments = {
          type                 = "object"
          required             = ["topic"]
          additionalProperties = false
          properties           = { topic = { type = "string", minLength = 1, maxLength = 80 } }
        }
      }
    }
  }
}

hook "mask_pii" {
  transport = "builtin"
  contract  = "transform"
}

interceptor "mask_guide" {
  surface  = "training"
  resource = "guide"
  stage    = "before_response"
  hook     = "mask_pii"
}

interceptor "check_topic" {
  surface = "training"
  prompt  = "review"
  stage   = "on_request"
  check   = "input.arguments.topic.matches('.*[^ ].*')"
}

interceptor "insert_topic" {
  surface   = "training"
  prompt    = "review"
  stage     = "before_response"
  order     = 10
  transform = "{'messages': [{'role': 'user', 'content': {'type': 'text', 'text': input.messages[0].content.text + ': ' + request.arguments.topic}}]}"
}
