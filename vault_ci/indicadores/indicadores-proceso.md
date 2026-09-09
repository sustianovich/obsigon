---
titulo: Indicadores de proceso
estado: vigente
fecha_vigencia: 2025-01-01
tags: [indicadores, calidad, sicc-2025]
---

# Indicadores de proceso

## Tabla de indicadores

| Codigo | Indicador | Objetivo | Fuente |
| --- | --- | --- | --- |
| IP-01 | Cobertura de invitacion | >= 95 % | Padron |
| IP-02 | Tasa de participacion | >= 70 % | Sistema |
| IP-03 | Tasa de recitacion | < 5 % | Radiologia |
| IP-04 | Demora media de lectura | <= 10 dias | Sistema |
| IP-05 | Discordancia entre lectores | 5-15 % | Radiologia |

## Calculo de la tasa de participacion
La tasa de participacion se calcula sobre invitaciones efectivamente
entregadas, no sobre invitaciones emitidas:

```python
def tasa_participacion(pruebas_realizadas, invitaciones_entregadas):
    """Devuelve la tasa en tanto por ciento."""
    if invitaciones_entregadas == 0:
        return 0.0
    return pruebas_realizadas / invitaciones_entregadas * 100
```

## Periodicidad
Los indicadores se calculan con periodicidad trimestral y se consolidan
anualmente en la memoria del programa. #indicadores
