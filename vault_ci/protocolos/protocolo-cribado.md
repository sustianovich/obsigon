---
titulo: Protocolo de cribado poblacional
estado: vigente
fecha_vigencia: 2025-01-01
fecha_revision: 2027-01-01
version: "3.2"
tags: [protocolo, cribado, sicc-2025]
responsable: Coordinacion del Programa
---

# Protocolo de cribado poblacional

> [!important] Ambito
> Este documento describe el circuito organizativo del programa. No
> sustituye al criterio clinico ni constituye indicacion individual.

## Poblacion diana
La invitacion se dirige a mujeres de 50 a 69 anos empadronadas en el area
de salud. El intervalo entre rondas es de veinticuatro meses. ^poblacion-diana

## Fases del circuito
El circuito consta de cuatro fases: invitacion, realizacion de la prueba,
lectura y comunicacion de resultados. La fase de lectura se detalla en
[[Circuito de lectura doble]].

| Fase | Responsable | Plazo maximo |
| --- | --- | --- |
| Invitacion | Unidad administrativa | 30 dias |
| Realizacion | Unidad de radiologia | 45 dias |
| Lectura | Radiologia (doble ciego) | 10 dias |
| Comunicacion | Coordinacion | 15 dias |

## Criterios de exclusion
Quedan excluidas las participantes con seguimiento activo en consulta
especializada y aquellas que hayan solicitado su baja voluntaria del
programa. Ver [[Gestion de exclusiones]].

## Codificacion de resultados
La codificacion sigue el estandar interno del programa:

```sql
-- Consulta de resultados pendientes de comunicacion
SELECT p.nia,
       p.fecha_prueba,
       r.categoria,
       r.fecha_lectura
FROM participantes p
JOIN resultados r ON r.nia = p.nia
WHERE r.categoria IN ('R3', 'R4', 'R5')
  AND r.fecha_comunicacion IS NULL
  AND r.fecha_lectura < DATE('now', '-15 days')
ORDER BY r.fecha_lectura ASC;
```

Las categorias R3, R4 y R5 activan el circuito descrito en
[[Circuito de valoracion adicional]]. #cribado #trazabilidad
